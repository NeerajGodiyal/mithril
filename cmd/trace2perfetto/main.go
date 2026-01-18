package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	pb "github.com/Overclock-Validator/mithril/cmd/trace2perfetto/proto"
	exptrace "golang.org/x/exp/trace"
	"google.golang.org/protobuf/proto"
)

func main() {
	inputFile := flag.String("input", "", "input Go trace file")
	outputFile := flag.String("output", "", "output Perfetto proto file")
	flag.Parse()

	if *inputFile == "" {
		log.Fatal("-input is required")
	}
	if *outputFile == "" {
		log.Fatalf("-output is required")
	}

	traceFile, err := os.Open(*inputFile)
	if err != nil {
		log.Fatalf("Failed to open trace file: %v", err)
	}
	defer traceFile.Close()

	r, err := exptrace.NewReader(traceFile)
	if err != nil {
		log.Fatalf("Failed to parse trace: %v", err)
	}

	trace, err := convertTrace(r)
	if err != nil {
		log.Fatalf("Failed to convert trace: %v", err)
	}

	data, err := proto.Marshal(trace)
	if err != nil {
		log.Fatalf("Failed to marshal proto: %v", err)
	}

	err = os.WriteFile(*outputFile, data, 0644)
	if err != nil {
		log.Fatalf("Failed to write output file: %v", err)
	}
	fmt.Fprintf(os.Stderr, "Wrote %d bytes to %s\n", len(data), *outputFile)
}

type trackID uint64

// trackAllocator manages Perfetto track assignment with reuse for short-lived goroutines.
type goroutineTrackAllocator struct {
	goroutineToTrack map[exptrace.GoID]trackID
	availableTracks  []trackID
}

func newGoroutineTrackAllocator() *goroutineTrackAllocator {
	return &goroutineTrackAllocator{
		goroutineToTrack: make(map[exptrace.GoID]trackID),
		availableTracks:  nil,
	}
}

// getOrCreateTrack returns a trackID and if it needs a Perfetto TrackDescriptor to be emitted.
func (a *goroutineTrackAllocator) getOrCreateTrack(g exptrace.GoID) (trackID, bool) {
	if t, ok := a.goroutineToTrack[g]; ok {
		return t, false
	}

	var t trackID
	if len(a.availableTracks) > 0 {
		t, a.availableTracks = a.availableTracks[len(a.availableTracks)-1], a.availableTracks[:len(a.availableTracks)-1]
		a.goroutineToTrack[g] = t
		return t, false
	}
	// Avoid 0 because protobuf
	// Avoid 1 because that's used for the process TrackDescriptor
	a.goroutineToTrack[g] = trackID(len(a.goroutineToTrack) + 2)
	return a.goroutineToTrack[g], true
}

func (a *goroutineTrackAllocator) releaseGoroutine(g exptrace.GoID) {
	if t, ok := a.goroutineToTrack[g]; ok {
		delete(a.goroutineToTrack, g)
		a.availableTracks = append(a.availableTracks, t)
	}
}

const ()

func convertTrace(r *exptrace.Reader) (*pb.Trace, error) {
	// TODO: associate runtime.Trace.Log with slices as DebugAnnotation?

	trace := &pb.Trace{}

	p := uint64(1)
	trace.Packet = append(trace.Packet, &pb.TracePacket{
		TrustedPacketSequenceId: 1,
		Data: &pb.TracePacket_TrackDescriptor{
			TrackDescriptor: &pb.TrackDescriptor{
				Uuid: p,
				Name: "mithril",
				Process: &pb.ProcessDescriptor{
					Pid:         1,
					ProcessName: "mithril",
				},
			},
		},
	})

	a := newGoroutineTrackAllocator()

	for {
		ev, err := r.ReadEvent()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("ReadEvent: %w", err)
		}
		var eventType pb.TrackEvent_Type
		var sliceName string
		var category string

		switch ev.Kind() {
		case exptrace.EventStateTransition:
			st := ev.StateTransition()
			if st.Resource.Kind == exptrace.ResourceGoroutine {
				_, to := st.Goroutine()
				// Release tracks for dead goroutines.
				if to == exptrace.GoNotExist {
					a.releaseGoroutine(st.Resource.Goroutine())
				}
			}
			continue
		case exptrace.EventRegionBegin:
			eventType = pb.TrackEvent_TYPE_SLICE_BEGIN
			sliceName = ev.Region().Type
			category = "region"
		case exptrace.EventRegionEnd:
			eventType = pb.TrackEvent_TYPE_SLICE_END
			sliceName = ev.Region().Type
			category = "region"
		case exptrace.EventTaskBegin:
			eventType = pb.TrackEvent_TYPE_SLICE_BEGIN
			sliceName = ev.Task().Type
			category = "task"
		case exptrace.EventTaskEnd:
			eventType = pb.TrackEvent_TYPE_SLICE_END
			sliceName = ev.Task().Type
			category = "task"
		default:
			continue
		}

		t, isNew := a.getOrCreateTrack(ev.Goroutine())
		if isNew {
			trace.Packet = append(trace.Packet, &pb.TracePacket{
				TrustedPacketSequenceId: 1,
				Data: &pb.TracePacket_TrackDescriptor{
					TrackDescriptor: &pb.TrackDescriptor{
						Uuid:       uint64(t),
						Name:       fmt.Sprintf("Track %03d", t),
						ParentUuid: p,
					},
				},
			})
		}

		trace.Packet = append(trace.Packet, &pb.TracePacket{
			Timestamp:               uint64(ev.Time()),
			TrustedPacketSequenceId: 1,
			Data: &pb.TracePacket_TrackEvent{
				TrackEvent: &pb.TrackEvent{
					TrackUuid:  uint64(t),
					Name:       sliceName,
					Type:       eventType,
					Categories: []string{category},
				},
			},
		})
	}

	return trace, nil
}
