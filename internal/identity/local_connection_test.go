package identity

import (
	"context"
	"net"
	"testing"
)

func TestLocalConnectionRequiresUnixSocketAndTrueBoolean(t *testing.T) {
	for _, value := range []any{false, "true", 1, struct{}{}, nil} {
		if LocalConnection(context.WithValue(context.Background(), localConnectionKey{}, value)) {
			t.Fatal("nonproof value accepted", value)
		}
	}
	if LocalConnection(context.Background()) {
		t.Fatal("absence is local")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() { c, _ := listener.Accept(); accepted <- c }()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn := <-accepted
	defer conn.Close()
	if LocalConnection(ConnectionContext(context.Background(), conn)) {
		t.Fatal("TCP trusted as local socket")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if LocalConnection(ConnectionContext(context.Background(), a)) {
		t.Fatal("every connection trusted")
	}
}
