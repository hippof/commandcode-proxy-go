package proxy

import (
	"net"
	"strconv"
	"testing"
)

func TestPickPortUsesRequestedWhenFree(t *testing.T) {
	// Find a free port, release it, and ask pickPort for exactly that one.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	got, fellBack := pickPort("127.0.0.1", port)
	if got != port || fellBack {
		t.Fatalf("pickPort = (%d,%v), want (%d,false)", got, fellBack, port)
	}
}

func TestPickPortFallsBackWhenTaken(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	taken := ln.Addr().(*net.TCPAddr).Port

	got, fellBack := pickPort("127.0.0.1", taken)
	if !fellBack {
		t.Fatalf("pickPort(%d) should report a fallback", taken)
	}
	if got == taken {
		t.Fatalf("pickPort returned the busy port %d", got)
	}
	// The fallback must actually be bindable.
	check, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(got))
	if err != nil {
		t.Fatalf("fallback port %d is not bindable: %v", got, err)
	}
	check.Close()
}

func TestCanBindReportsBusyPorts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	if canBind("127.0.0.1", busy) {
		t.Fatalf("canBind(%d) = true for a busy port", busy)
	}
}
