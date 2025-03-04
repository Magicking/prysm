package encoder

import (
	"io"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prysmaticlabs/go-ssz"
)

var _ = NetworkEncoding(&SszNetworkEncoder{})

// SszNetworkEncoder supports p2p networking encoding using SimpleSerialize
// with snappy compression (if enabled).
type SszNetworkEncoder struct {
	UseSnappyCompression bool
}

// Encode the proto message to the io.Writer. This encoding prefixes the byte slice with a protobuf varint
// to indicate the size of the message.
func (e SszNetworkEncoder) Encode(w io.Writer, msg proto.Message) (int, error) {
	if msg == nil {
		return 0, nil
	}

	b, err := ssz.Marshal(msg)
	if err != nil {
		return 0, err
	}
	if e.UseSnappyCompression {
		b = snappy.Encode(nil /*dst*/, b)
	}
	b = append(proto.EncodeVarint(uint64(len(b))), b...)
	return w.Write(b)
}

// Decode the bytes from io.Reader to the protobuf message provided.
func (e SszNetworkEncoder) Decode(r io.Reader, to proto.Message) error {
	msgLen, err := readVarint(r)
	if err != nil {
		return err
	}
	b := make([]byte, msgLen)
	_, err = r.Read(b)
	if err != nil {
		return err
	}
	if e.UseSnappyCompression {
		var err error
		b, err = snappy.Decode(nil /*dst*/, b)
		if err != nil {
			return err
		}
	}

	return ssz.Unmarshal(b, to)
}

// ProtocolSuffix returns the appropriate suffix for protocol IDs.
func (e SszNetworkEncoder) ProtocolSuffix() string {
	if e.UseSnappyCompression {
		return "/ssz_snappy"
	}
	return "/ssz"
}
