package proto

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestNegotiate(t *testing.T) {
	cases := []struct {
		peer    []int
		want    int
		wantErr bool
	}{
		{[]int{1}, 1, false},
		{[]int{1, 2, 99}, 1, false}, // picks highest shared, ignores unknown
		{[]int{99}, 0, true},
		{[]int{0}, 0, true}, // below floor
		{nil, 0, true},
	}
	for _, c := range cases {
		got, err := Negotiate(c.peer)
		if c.wantErr != (err != nil) || got != c.want {
			t.Errorf("Negotiate(%v) = %d, %v; want %d, err=%v", c.peer, got, err, c.want, c.wantErr)
		}
	}
}

func TestConnRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(struct {
		io.Reader
		io.Writer
	}{&buf, &buf})

	hello := Hello{AgentID: "a1", Credential: "secret", ProtoVersions: []int{1}}
	if err := c.Send(TypeHello, "c-1", hello); err != nil {
		t.Fatal(err)
	}
	env, err := c.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeHello || env.Corr != "c-1" {
		t.Fatalf("envelope = %+v", env)
	}
	got, err := Decode[Hello](env)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "a1" || got.ProtoVersions[0] != 1 {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestRecvRejectsOversized(t *testing.T) {
	big := `{"type":"announce","payload":"` + strings.Repeat("x", MaxControlMessage) + `"}` + "\n"
	c := NewConn(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(big), io.Discard})
	_, err := c.Recv()
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
}

func TestRecvRejectsMissingType(t *testing.T) {
	c := NewConn(struct {
		io.Reader
		io.Writer
	}{strings.NewReader("{}\n"), io.Discard})
	if _, err := c.Recv(); err == nil {
		t.Fatal("want error for missing type")
	}
}

// The open-header exchange must leave the reader positioned exactly at the
// first raw byte after the header line, even when raw bytes arrive in the
// same read.
func TestHeaderFrameThenRawBytes(t *testing.T) {
	var wire bytes.Buffer
	if err := WriteHeaderFrame(&wire, OpenRequest{Corr: "c-2", DeviceID: "amber-falcon", Params: OpenParams{Baud: 115200}}); err != nil {
		t.Fatal(err)
	}
	raw := []byte{0x00, 0x01, '\n', 0xFF, 'x'}
	wire.Write(raw)

	br := bufio.NewReader(&wire)
	var req OpenRequest
	if err := ReadHeaderFrame(br, &req); err != nil {
		t.Fatal(err)
	}
	if req.DeviceID != "amber-falcon" || req.Params.Baud != 115200 {
		t.Fatalf("req = %+v", req)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, raw) {
		t.Fatalf("raw tail = %v, want %v", rest, raw)
	}
}
