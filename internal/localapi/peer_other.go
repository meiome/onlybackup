//go:build !linux

package localapi

import (
	"context"
	"net"
)

type Peer struct{ PID, UID, GID int }
type CredentialListener struct {
	net.Listener
	Allow func(Peer) bool
}

func (l CredentialListener) Accept() (net.Conn, error)            { return l.Listener.Accept() }
func ConnContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
func PeerFromContext(context.Context) (Peer, bool)                { return Peer{}, false }
