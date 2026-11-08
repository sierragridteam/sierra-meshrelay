package relay

import (
	"time"

	"github.com/sierragridteam/sierra-meshrelay/meshproto"
)

// The wire protocol's types and constants live in the shared module
// package meshproto (spec: PROTOCOL.md at the repo root), which
// the bridge imports too. The names below alias it so the relay code reads as
// before.

// Subprotocol names carry the protocol's major version.
const (
	BridgeSubprotocol = meshproto.BridgeSubprotocol
	ClientSubprotocol = meshproto.ClientSubprotocol
	ProtocolVersion   = meshproto.Version
)

// MaxFrameBytes is the largest binary message (one companion frame payload)
// either side may send. Current firmware's max frame is 176 bytes.
const MaxFrameBytes = meshproto.MaxFrameBytes

// maxControlBytes bounds a text (JSON control) message. Control messages are
// tiny; this only has to clear a hello with long names in it.
const maxControlBytes = meshproto.MaxControlBytes

// Close codes. See the wire protocol's close-code table.
const (
	CloseNormal          = meshproto.CloseNormal
	CloseGoingAway       = meshproto.CloseGoingAway
	CloseBadToken        = meshproto.CloseBadToken
	CloseGatewayMismatch = meshproto.CloseGatewayMismatch
	CloseProtocolError   = meshproto.CloseProtocolError
	CloseUnsupported     = meshproto.CloseUnsupported
	CloseReplaced        = meshproto.CloseReplaced
	CloseBusy            = meshproto.CloseBusy
)

// Node states as reported by the bridge.
const (
	NodeConnected    = meshproto.NodeConnected
	NodeDisconnected = meshproto.NodeDisconnected
	NodeMismatch     = meshproto.NodeMismatch
	NodePaused       = meshproto.NodePaused
)

// lease_ending reasons. Receivers must treat an unknown reason as the lease
// ending.
const (
	ReasonExpiry         = meshproto.ReasonExpiry
	ReasonPreempted      = meshproto.ReasonPreempted
	ReasonGatewayOffline = meshproto.ReasonGatewayOffline
	ReasonIdle           = meshproto.ReasonIdle
	ReasonRevoked        = meshproto.ReasonRevoked
	ReasonPubkeyMismatch = meshproto.ReasonPubkeyMismatch
	ReasonShutdown       = meshproto.ReasonRelayShutdown
	ReasonHostPaused     = meshproto.ReasonHostPaused
	// reasonClientClosed is audit-only: the holder hung up, nobody is told.
	reasonClientClosed = "client_closed"
)

// Message shapes.
type (
	envelope         = meshproto.Envelope
	Agent            = meshproto.Agent
	Link             = meshproto.Link
	NodeInfo         = meshproto.Node
	helloMsg         = meshproto.Hello
	welcomeMsg       = meshproto.Welcome
	nodeStatusMsg    = meshproto.NodeStatus
	Stats            = meshproto.Stats
	statsMsg         = meshproto.StatsMsg
	attachMsg        = meshproto.Attach
	detachMsg        = meshproto.Detach
	typeOnlyMsg      = meshproto.Envelope
	errorMsg         = meshproto.Error
	leaseMsg         = meshproto.Lease
	leaseEndingMsg   = meshproto.LeaseEnding
	renewMsg         = meshproto.Renew
	gatewayStatusMsg = meshproto.GatewayStatus
)

func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
