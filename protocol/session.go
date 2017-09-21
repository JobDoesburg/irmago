package protocol

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/credentials/irmago"
	"github.com/mhe/gabi"
)

// PermissionHandler is a callback for providing permission for an IRMA session
// and specifying the attributes to be disclosed.
type PermissionHandler func(proceed bool, choice *irmago.DisclosureChoice)

// A Handler contains callbacks for communication to the user.
type Handler interface {
	StatusUpdate(action Action, status Status)
	Success(action Action)
	Cancelled(action Action)
	Failure(action Action, err *irmago.Error)
	UnsatisfiableRequest(action Action, missing irmago.AttributeDisjunctionList)

	AskIssuancePermission(request irmago.IssuanceRequest, ServerName string, callback PermissionHandler)
	AskVerificationPermission(request irmago.DisclosureRequest, ServerName string, callback PermissionHandler)
	AskSignaturePermission(request irmago.SignatureRequest, ServerName string, callback PermissionHandler)

	AskPin(remainingAttempts int, callback func(pin string))
}

// A session is an IRMA session.
type session struct {
	Action    Action
	Version   Version
	ServerURL string
	Handler   Handler

	jwt         RequestorJwt
	irmaSession irmago.Session
	transport   *irmago.HTTPTransport
	choice      *irmago.DisclosureChoice
}

// Supported protocol versions. Minor version numbers should be reverse sorted.
var supportedVersions = map[int][]int{
	2: {2, 1},
}

func calcVersion(qr *Qr) (string, error) {
	// Parse range supported by server
	var minmajor, minminor, maxmajor, maxminor int
	var err error
	if minmajor, err = strconv.Atoi(string(qr.ProtocolVersion[0])); err != nil {
		return "", err
	}
	if minminor, err = strconv.Atoi(string(qr.ProtocolVersion[2])); err != nil {
		return "", err
	}
	if maxmajor, err = strconv.Atoi(string(qr.ProtocolMaxVersion[0])); err != nil {
		return "", err
	}
	if maxminor, err = strconv.Atoi(string(qr.ProtocolMaxVersion[2])); err != nil {
		return "", err
	}

	// Iterate supportedVersions in reverse sorted order (i.e. biggest major number first)
	keys := make([]int, 0, len(supportedVersions))
	for k := range supportedVersions {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	for _, major := range keys {
		for _, minor := range supportedVersions[major] {
			aboveMinimum := major > minmajor || (major == minmajor && minor >= minminor)
			underMaximum := major < maxmajor || (major == maxmajor && minor <= maxminor)
			if aboveMinimum && underMaximum {
				return fmt.Sprintf("%d.%d", major, minor), nil
			}
		}
	}
	return "", fmt.Errorf("No supported protocol version between %s and %s", qr.ProtocolVersion, qr.ProtocolMaxVersion)
}

// NewSession creates and starts a new IRMA session.
func NewSession(qr *Qr, handler Handler) {
	version, err := calcVersion(qr)
	if err != nil {
		handler.Failure(ActionUnknown, &irmago.Error{ErrorCode: irmago.ErrorProtocolVersionNotSupported, Err: err})
		return
	}

	session := &session{
		Version:   Version(version),
		Action:    Action(qr.Type),
		ServerURL: qr.URL,
		Handler:   handler,
		transport: irmago.NewHTTPTransport(qr.URL),
	}

	// Check if the action is one of the supported types
	switch session.Action {
	case ActionDisclosing: // nop
	case ActionSigning: // nop
	case ActionIssuing: // nop
	case ActionUnknown:
		fallthrough
	default:
		handler.Failure(ActionUnknown, &irmago.Error{ErrorCode: irmago.ErrorUnknownAction, Err: nil, Info: string(session.Action)})
		return
	}

	if !strings.HasSuffix(session.ServerURL, "/") {
		session.ServerURL += "/"
	}

	go session.start()

	return
}

// start retrieves the first message in the IRMA protocol, checks if we can perform
// the request, and informs the user of the outcome.
func (session *session) start() {
	session.Handler.StatusUpdate(session.Action, StatusCommunicating)

	// Get the first IRMA protocol message and parse it
	info := &SessionInfo{}
	Err := session.transport.Get("jwt", info)
	if Err != nil {
		session.Handler.Failure(session.Action, Err.(*irmago.Error))
		return
	}

	switch session.Action {
	case ActionDisclosing:
		session.jwt = &ServiceProviderJwt{}
	case ActionSigning:
		session.jwt = &SignatureRequestorJwt{}
	case ActionIssuing:
		session.jwt = &IdentityProviderJwt{}
	default:
		panic("Invalid session type") // does not happen, session.Action has been checked earlier
	}
	server, err := irmago.JwtDecode(info.Jwt, session.jwt)
	if err != nil {
		session.Handler.Failure(session.Action, &irmago.Error{ErrorCode: irmago.ErrorInvalidJWT, Err: err})
		return
	}
	session.irmaSession = session.jwt.IrmaSession()
	session.irmaSession.SetContext(info.Context)
	session.irmaSession.SetNonce(info.Nonce)
	if session.Action == ActionIssuing {
		// Store which public keys the server will use
		for _, credreq := range session.irmaSession.(*irmago.IssuanceRequest).Credentials {
			credreq.KeyCounter = info.Keys[credreq.Credential.IssuerIdentifier()]
		}
	}

	missing := irmago.Manager.CheckSatisfiability(session.irmaSession.DisjunctionList())
	if len(missing) > 0 {
		session.Handler.UnsatisfiableRequest(session.Action, missing)
		return
	}

	// Ask for permission to execute the session
	callback := PermissionHandler(func(proceed bool, choice *irmago.DisclosureChoice) {
		session.choice = choice
		session.irmaSession.SetDisclosureChoice(choice)
		go session.do(proceed)
	})
	session.Handler.StatusUpdate(session.Action, StatusConnected)
	switch session.Action {
	case ActionDisclosing:
		session.Handler.AskVerificationPermission(*session.irmaSession.(*irmago.DisclosureRequest), server, callback)
	case ActionSigning:
		session.Handler.AskSignaturePermission(*session.irmaSession.(*irmago.SignatureRequest), server, callback)
	case ActionIssuing:
		session.Handler.AskIssuancePermission(*session.irmaSession.(*irmago.IssuanceRequest), server, callback)
	default:
		panic("Invalid session type") // does not happen, session.Action has been checked earlier
	}
}

func (session *session) do(proceed bool) {
	if !proceed {
		session.Handler.Cancelled(session.Action)
		return
	}
	session.Handler.StatusUpdate(session.Action, StatusCommunicating)

	if !session.irmaSession.Distributed() {
		var message interface{}
		var err error
		switch session.Action {
		case ActionSigning:
			message, err = irmago.Manager.Proofs(session.choice, session.irmaSession, true)
		case ActionDisclosing:
			message, err = irmago.Manager.Proofs(session.choice, session.irmaSession, false)
		case ActionIssuing:
			message, err = irmago.Manager.IssueCommitments(session.irmaSession.(*irmago.IssuanceRequest))
		}
		if err != nil {
			session.Handler.Failure(session.Action, &irmago.Error{ErrorCode: irmago.ErrorCrypto, Err: err})
			return
		}
		session.sendResponse(message)
	} else {
		var builders []gabi.ProofBuilder
		var err error
		switch session.Action {
		case ActionSigning:
			fallthrough
		case ActionDisclosing:
			builders, err = irmago.Manager.ProofBuilders(session.choice)
		case ActionIssuing:
			builders, err = irmago.Manager.IssuanceProofBuilders(session.irmaSession.(*irmago.IssuanceRequest))
		}
		if err != nil {
			session.Handler.Failure(session.Action, &irmago.Error{ErrorCode: irmago.ErrorCrypto, Err: err})
		}

		irmago.StartKeyshareSession(session.irmaSession, builders, session)
	}
}

func (session *session) AskPin(remainingAttempts int, callback func(pin string)) {
	session.Handler.AskPin(remainingAttempts, callback)
}

func (session *session) KeyshareDone(message interface{}) {
	session.sendResponse(message)
}

func (session *session) KeyshareBlocked(duration int) {
	session.Handler.Failure(
		session.Action,
		&irmago.Error{ErrorCode: irmago.ErrorKeyshareBlocked, Info: strconv.Itoa(duration)},
	)
}

func (session *session) KeyshareError(err error) {
	session.Handler.Failure(session.Action, &irmago.Error{ErrorCode: irmago.ErrorKeyshare, Err: err})
}

func (session *session) sendResponse(message interface{}) {
	var err error
	switch session.Action {
	case ActionSigning:
		fallthrough
	case ActionDisclosing:
		var response string
		if err = session.transport.Post("proofs", &response, message); err != nil {
			session.Handler.Failure(session.Action, err.(*irmago.Error))
			return
		}
		if response != "VALID" {
			session.Handler.Failure(session.Action, &irmago.Error{ErrorCode: irmago.ErrorRejected, Info: response})
			return
		}
	case ActionIssuing:
		response := []*gabi.IssueSignatureMessage{}
		if err = session.transport.Post("commitments", &response, message); err != nil {
			session.Handler.Failure(session.Action, err.(*irmago.Error))
			return
		}
		if err = irmago.Manager.ConstructCredentials(response, session.irmaSession.(*irmago.IssuanceRequest)); err != nil {
			session.Handler.Failure(session.Action, &irmago.Error{Err: err, ErrorCode: irmago.ErrorCrypto})
			return
		}
	}

	session.Handler.Success(session.Action)
}
