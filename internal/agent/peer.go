package agent

import "context"

// Peer records the authenticated identity associated with a session.
type Peer struct {
	Transport string `json:"transport"`
	LoginName string `json:"login_name,omitempty"`
	NodeName  string `json:"node_name,omitempty"`
	Address   string `json:"address,omitempty"`
	LocalUser string `json:"local_user,omitempty"`
}

type peerKey struct{}

func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}
func PeerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}
