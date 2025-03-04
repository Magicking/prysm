package sync

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	libp2pcore "github.com/libp2p/go-libp2p-core"
	"github.com/libp2p/go-libp2p-core/network"
	p2ptest "github.com/prysmaticlabs/prysm/beacon-chain/p2p/testing"
	pb "github.com/prysmaticlabs/prysm/proto/testing"
	"github.com/prysmaticlabs/prysm/shared/testutil"
)

// expectSuccess status code from a stream in regular sync.
func expectSuccess(t *testing.T, r *RegularSync, stream network.Stream) {
	code, errMsg, err := r.readStatusCode(stream)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("Received non-zero response code: %d", code)
	}
	if errMsg != nil {
		t.Fatalf("Received error message from stream: %+v", errMsg)
	}
}

func TestRegisterRPC_ReceivesValidMessage(t *testing.T) {
	p2p := p2ptest.NewTestP2P(t)
	r := &RegularSync{
		ctx: context.Background(),
		p2p: p2p,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	topic := "/testing/foobar/1"
	handler := func(ctx context.Context, msg proto.Message, stream libp2pcore.Stream) error {
		m := msg.(*pb.TestSimpleMessage)
		if !bytes.Equal(m.Foo, []byte("foo")) {
			t.Errorf("Unexpected incoming message: %+v", m)
		}
		wg.Done()

		return nil
	}
	r.registerRPC(topic, &pb.TestSimpleMessage{}, handler)

	p2p.ReceiveRPC(topic, &pb.TestSimpleMessage{Foo: []byte("foo")})

	if testutil.WaitTimeout(&wg, time.Second) {
		t.Fatal("Did not receive RPC in 1 second")
	}
}
