package util

import (
	"net"
	"time"

	mkdns "github.com/miekg/dns"
)

// DNSResult is the middleware that connects the packet encoder to Any output.
// For DNStap, this is probably going to be replaced with something else.
type DNSResult struct {
	Timestamp    time.Time
	DNS          mkdns.Msg
	IPVersion    uint8
	SrcIP        net.IP
	SrcPort      uint16 `json:",omitempty"`
	DstIP        net.IP
	DstPort      uint16 `json:",omitempty"`
	Protocol     string
	PacketLength uint16
	Identity     string `json:",omitempty"`
	Version      string `json:",omitempty"`
}

type GenericOutput interface {
	Initialize() error             // try to initialize the output by checking flags and connections
	Output()                       // the output is a goroutine that fetches data from the registered channel and pushes it to output, possibly in multiple workers
	OutputChannel() chan DNSResult // returns the output channel associated with the output
	Close()                        // close down the connections and exit cleanly
}

type OutputMarshaller interface {
	Marshal(d DNSResult) string // marshal the DNSResult into the output format
	Init() (string, error)      // initialize the output format
}

var GlobalDispatchList = make([]GenericOutput, 0, 1024) // 1024 outputs is an absurdly high number
