//go:build linux

package localapi

import (
	"context"
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

type Peer struct {
	PID int `json:"pid"`
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type peerKey struct{}

type credentialConn struct {
	net.Conn
	peer Peer
}

type CredentialListener struct {
	net.Listener
	Allow func(Peer) bool
}

func (l CredentialListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			conn.Close()
			return nil, errors.New("socket privilegiata non Unix")
		}
		raw, err := unixConn.SyscallConn()
		if err != nil {
			conn.Close()
			continue
		}
		var credential *unix.Ucred
		var controlErr error
		err = raw.Control(func(fd uintptr) {
			credential, controlErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		})
		if err != nil || controlErr != nil || credential == nil {
			conn.Close()
			continue
		}
		peer := Peer{PID: int(credential.Pid), UID: int(credential.Uid), GID: int(credential.Gid)}
		if l.Allow != nil && !l.Allow(peer) {
			conn.Close()
			continue
		}
		return &credentialConn{Conn: conn, peer: peer}, nil
	}
}

func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	if credential, ok := conn.(*credentialConn); ok {
		return context.WithValue(ctx, peerKey{}, credential.peer)
	}
	return ctx
}

func PeerFromContext(ctx context.Context) (Peer, bool) {
	peer, ok := ctx.Value(peerKey{}).(Peer)
	return peer, ok
}
