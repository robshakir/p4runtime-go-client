package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	p4_v1 "github.com/p4lang/p4runtime/go/p4/v1"

	"github.com/antoninbas/p4runtime-go-client/pkg/client"
	"github.com/antoninbas/p4runtime-go-client/pkg/signals"

	"inet.af/netaddr"
)

const (
	defaultDeviceID = 0
)

var (
	defaultAddr = fmt.Sprintf("127.0.0.1:%d", client.P4RuntimePort)
)

func main() {
	ctx := context.Background()

	var addr string
	flag.StringVar(&addr, "addr", defaultAddr, "P4Runtime server socket")
	var deviceID uint64
	flag.Uint64Var(&deviceID, "device-id", defaultDeviceID, "Device id")
	var binPath string
	flag.StringVar(&binPath, "bin", "", "Path to P4 bin")
	var p4infoPath string
	flag.StringVar(&p4infoPath, "p4info", "", "Path to P4Info")

	flag.Parse()

	if binPath == "" || p4infoPath == "" {
		log.Fatalf("Missing .bin or P4Info")
	}

	log.Infof("Connecting to server at %s", addr)
	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Cannot connect to server: %v", err)
	}
	defer conn.Close()

	c := p4_v1.NewP4RuntimeClient(conn)
	resp, err := c.Capabilities(ctx, &p4_v1.CapabilitiesRequest{})
	if err != nil {
		log.Fatalf("Error in Capabilities RPC: %v", err)
	}
	log.Infof("P4Runtime server version is %s", resp.P4RuntimeApiVersion)

	stopCh := signals.RegisterSignalHandlers()

	electionID := p4_v1.Uint128{High: 0, Low: 1}

	p4RtC := client.NewClient(c, deviceID, electionID)
	arbitrationCh := make(chan bool)
	go p4RtC.Run(stopCh, arbitrationCh, nil)

	waitCh := make(chan struct{})

	go func() {
		sent := false
		for isPrimary := range arbitrationCh {
			if isPrimary {
				log.Infof("We are the primary client!")
				if !sent {
					waitCh <- struct{}{}
					sent = true
				}
			} else {
				log.Infof("We are not the primary client!")
			}
		}
	}()

	func() {
		timeout := 5 * time.Second
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		select {
		case <-ctx.Done():
			log.Fatalf("Could not become the primary client within %v", timeout)
		case <-waitCh:
		}
	}()

	log.Info("Setting forwarding pipe")
	if _, err := p4RtC.SetFwdPipe(ctx, binPath, p4infoPath, 0); err != nil {
		log.Fatalf("Error when setting forwarding pipe: %v", err)
	}

	bh := netaddr.MustParseIP("192.0.2.1").As4()
	blackhole := p4RtC.NewTableEntry(
		"MyIngress.ipv4_lpm", 	// table name
		[]client.MatchInterface{&client.LpmMatch{
			Value: []byte(bh[:]),
			PLen: 32,
		}},
		p4RtC.NewTableActionDirect("MyIngress.drop", nil),
		nil,
	)


	nh := netaddr.MustParseIP("192.0.2.2").As4()
	fwd := p4RtC.NewTableEntry(
		"MyIngress.ipv4_lpm",
		[]client.MatchInterface{&client.LpmMatch{
			Value: []byte(nh[:]),
			PLen: 32,
		}},
		// forward parameters
		p4RtC.NewTableActionDirect("MyIngress.ipv4_forward", [][]byte{
			[]byte{3,2,1,0,0,0}, // 6bytes of MAC
			[]byte{1}, // egress port ID
		}),
		nil,
	)

	zero := netaddr.MustParseIP("0.0.0.0").As4()
	def := p4RtC.NewTableEntry(
		"MyIngress.ipv4_lpm",
		[]client.MatchInterface{&client.LpmMatch{
			Value: []byte(zero[:]),
			PLen: 1, 
		}},
		p4RtC.NewTableActionDirect("MyIngress.drop", nil),
		nil,
	)

	onetwentyeight := netaddr.MustParseIP("128.0.0.0").As4()
	z128 := p4RtC.NewTableEntry(
		"MyIngress.ipv4_lpm",
		[]client.MatchInterface{&client.LpmMatch{
			Value: []byte(onetwentyeight[:]),
			PLen: 1, 
		}},
		p4RtC.NewTableActionDirect("MyIngress.drop", nil),
		nil,
	)

	for _, e := range []*p4_v1.TableEntry{blackhole, fwd, def, z128}{
		if err := p4RtC.InsertTableEntry(ctx, e); err != nil {
			log.Errorf("cannot create entry, err: %v", err)
		}
	}

	log.Info("Do Ctrl-C to quit")
	<-stopCh
	log.Info("Stopping client")
}
