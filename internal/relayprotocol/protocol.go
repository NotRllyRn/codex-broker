package relayprotocol

import "github.com/NotRllyRn/codex-broker/internal/gateway"

const Version = 5

type Client struct {
	ID, Name, Prefix  string
	Digest            []byte
	Restricted        bool
	StreamerMode      bool
	AllowedAccountIDs []string
}

type Lease struct {
	gateway.Lease
	CycleState        string
	CreatedAtMS       int64
	Reserve           bool
	PermitEpoch       string
	PermitExpiresAtMS int64
}

type Snapshot struct {
	ProtocolVersion                int
	SnapshotID, RelayID, PoolEpoch string
	GeneratedAtMS                  int64
	Clients                        []Client
	Leases                         []Lease
}

type RouteRequest struct {
	ClientID string
	Request  gateway.RouteRequest
}

type RouteResponse struct {
	Lease *gateway.Lease
	Wait  *gateway.PoolWait
}

type Event struct {
	ID, RelayID, Kind, AccountID, ClientID, FailureKind, PermitEpoch string
	OccurredAtMS                                                     int64
	Observation                                                      *gateway.Observation
}

type EventBatch struct{ Events []Event }

type Heartbeat struct {
	RelayID, SnapshotID string
	ProtocolVersion     int
	CacheGeneratedAtMS  int64
	Ready               bool
}
