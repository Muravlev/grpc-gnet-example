package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"sync"

	echo "github.com/Muravlev/grpc-gnet-example/proto"
	"github.com/panjf2000/gnet/v2"
	"github.com/panjf2000/gnet/v2/pkg/pool/goroutine"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
	"google.golang.org/protobuf/proto"
)

type grpcServer struct {
	gnet.BuiltinEventEngine
	addr string
}

type grpcCodec struct {
	server       *grpcServer
	streams      map[uint32]*grpcStream
	prefaceDone  bool
	hpackBuf     *bytes.Buffer
	hpackEncoder *hpack.Encoder
}

type grpcStream struct {
	id         uint32
	buf        bytes.Buffer
	clientDone bool
}

const (
	http2Preface  = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	maxBufferSize = 1024 * 1024 // 1MB max buffer size
)

var bufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func (s *grpcServer) OnOpen(c gnet.Conn) ([]byte, gnet.Action) {

	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	encoder := hpack.NewEncoder(buf)

	codec := &grpcCodec{
		server:       s,
		streams:      make(map[uint32]*grpcStream),
		hpackBuf:     buf,
		hpackEncoder: encoder,
	}
	c.SetContext(codec)
	return nil, gnet.None
}

func (s *grpcServer) OnTraffic(c gnet.Conn) gnet.Action {
	codec := c.Context().(*grpcCodec)

	if !codec.prefaceDone {
		preface, err := c.Peek(len(http2Preface))
		if err != nil || len(preface) < len(http2Preface) {
			return gnet.None
		}

		if !bytes.Equal(preface, []byte(http2Preface)) {
			log.Println("[grpc] Invalid HTTP/2 preface")
			return gnet.Close
		}

		c.Discard(len(http2Preface))
		codec.prefaceDone = true
	}

	buf, err := c.Peek(-1)
	if err != nil {
		log.Println("[grpc] Error peeking data:", err)
		return gnet.Close
	}
	reader := bytes.NewReader(buf)
	framer := http2.NewFramer(nil, reader)

	for reader.Len() >= 9 {
		frame, err := framer.ReadFrame()
		if err != nil {
			break
		}

		switch f := frame.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				sendSettingsAck(c)
			}

		case *http2.HeadersFrame:
			stream := codec.streams[f.StreamID]
			if stream == nil {
				stream = &grpcStream{id: f.StreamID, buf: *bufferPool.Get().(*bytes.Buffer)}
				codec.streams[f.StreamID] = stream
			}
			if f.StreamEnded() {
				stream.clientDone = true
			}

			if stream.clientDone && stream.buf.Len() >= 5 {
				data := append([]byte(nil), stream.buf.Bytes()...) // копируем буфер
				streamID := stream.id
				cleanupStream(codec, f.StreamID)
				_ = workerPool.Submit(func() {
					if !processStream(c, streamID, data, codec) {
						sendGrpcError(c, stream.id)
					}
				})

			}

		case *http2.DataFrame:
			stream := codec.streams[f.StreamID]
			if stream == nil {
				sendRstStream(c, f.StreamID, http2.ErrCodeStreamClosed)
				continue
			}

			if stream.buf.Len()+len(f.Data()) > maxBufferSize {
				sendRstStream(c, f.StreamID, http2.ErrCodeFlowControl)
				cleanupStream(codec, f.StreamID)
				continue
			}

			stream.buf.Write(f.Data())
			if f.StreamEnded() {
				stream.clientDone = true
			}

			if stream.clientDone && stream.buf.Len() >= 5 {
				data := append([]byte(nil), stream.buf.Bytes()...) // копируем буфер
				streamID := stream.id
				cleanupStream(codec, f.StreamID)
				_ = workerPool.Submit(func() {
					if !processStream(c, streamID, data, codec) {
						sendGrpcError(c, stream.id)
					}
				})
			}

		case *http2.PingFrame:
			if !f.Flags.Has(http2.FlagPingAck) {
				_ = workerPool.Submit(func() {
					var out bytes.Buffer
					fr := http2.NewFramer(&out, nil)
					fr.WritePing(true, f.Data)
					c.AsyncWrite(out.Bytes(), nil)
				})
			}

		case *http2.WindowUpdateFrame:
		case *http2.GoAwayFrame:
			return gnet.Close

		case *http2.RSTStreamFrame:
			cleanupStream(codec, f.Header().StreamID)

		default:
			log.Printf("❌ RST_STREAM received for stream %d", f.Header().StreamID)
			log.Println("[grpc] Unknown frame type:", f.Header().Type)
			cleanupStream(codec, f.Header().StreamID)
			c.Close()
		}
	}
	discarded := len(buf) - reader.Len()
	c.Discard(discarded)
	return gnet.None
}

func (s *grpcServer) OnClose(c gnet.Conn, err error) gnet.Action {
	codec := c.Context().(*grpcCodec)
	for _, stream := range codec.streams {
		stream.buf.Reset()
		bufferPool.Put(&stream.buf)
	}
	codec.streams = nil
	codec.hpackBuf.Reset()
	bufferPool.Put(codec.hpackBuf)
	c.SetContext(nil)
	return gnet.None
}

// func (s *grpcServer) OnShutdown(sv gnet.Server) {
// 	if s.workerPool != nil {
// 		s.workerPool.Release()
// 	}
// }

var workerPool = goroutine.Default()

func main() {
	var port int
	var multicore bool
	var loops int
	var metricsPort int

	flag.IntVar(&port, "port", 9080, "server port")
	flag.BoolVar(&multicore, "multicore", true, "enable multicore")
	flag.IntVar(&loops, "eventloops", 4, "number of event loops")
	flag.IntVar(&metricsPort, "metrics-port", 8081, "metrics server port")
	flag.Parse()

	go func() {
		metricsHandler := fasthttpadaptor.NewFastHTTPHandler(promhttp.Handler())
		metricsServer := &fasthttp.Server{
			Handler: metricsHandler,
		}
		log.Printf("Starting metrics server on :%d", metricsPort)
		if err := metricsServer.ListenAndServe(fmt.Sprintf(":%d", metricsPort)); err != nil {
			log.Fatalf("Error starting metrics server: %v", err)
		}
	}()

	addr := fmt.Sprintf("tcp://0.0.0.0:%d", port)
	server := &grpcServer{
		addr: addr,
	}

	log.Fatal(gnet.Run(
		server,
		addr,
		gnet.WithTCPNoDelay(gnet.TCPNoDelay),
		gnet.WithReusePort(true),
		gnet.WithMulticore(multicore),
		gnet.WithNumEventLoop(loops),
	))
}

func sendSettingsAck(c gnet.Conn) {
	_ = workerPool.Submit(func() {
		var out bytes.Buffer
		fr := http2.NewFramer(&out, nil)
		fr.WriteSettingsAck()
		c.AsyncWrite(out.Bytes(), nil)
	})
}

func processStream(c gnet.Conn, streamID uint32, payload []byte, codec *grpcCodec) bool {
	if len(payload) < 5 {
		return false
	}
	msgLen := binary.BigEndian.Uint32(payload[1:5])
	if int(msgLen)+5 > len(payload) {
		return false
	}

	var req echo.EchoRequest
	if err := proto.Unmarshal(payload[5:5+msgLen], &req); err != nil {
		log.Println("[grpc] Error unmarshalling request:", err)
		return false
	}
	resp := &echo.EchoResponse{Message: "Echo: " + req.Message}
	respBytes, _ := proto.Marshal(resp)

	var grpcOut bytes.Buffer
	grpcOut.WriteByte(0)
	binary.Write(&grpcOut, binary.BigEndian, uint32(len(respBytes)))
	grpcOut.Write(respBytes)

	var out bytes.Buffer
	fr := http2.NewFramer(&out, nil)
	codec.hpackBuf.Reset()
	codec.hpackEncoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
	codec.hpackEncoder.WriteField(hpack.HeaderField{Name: "content-type", Value: "application/grpc"})
	fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		EndHeaders:    true,
		EndStream:     false,
		BlockFragment: codec.hpackBuf.Bytes(),
	})
	fr.WriteData(streamID, false, grpcOut.Bytes())
	codec.hpackBuf.Reset()
	codec.hpackEncoder.WriteField(hpack.HeaderField{Name: "grpc-status", Value: "0"})
	fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		EndHeaders:    true,
		EndStream:     true,
		BlockFragment: codec.hpackBuf.Bytes(),
	})

	c.AsyncWrite(out.Bytes(), nil)

	return true
}

func cleanupStream(codec *grpcCodec, streamID uint32) {
	if stream := codec.streams[streamID]; stream != nil {
		stream.buf.Reset()
		bufferPool.Put(&stream.buf)
		delete(codec.streams, streamID)
	}
}

func sendRstStream(c gnet.Conn, streamID uint32, code http2.ErrCode) {
	_ = workerPool.Submit(func() {
		var out bytes.Buffer
		fr := http2.NewFramer(&out, nil)
		fr.WriteRSTStream(streamID, code)
		c.AsyncWrite(out.Bytes(), nil)
	})
}

func sendGrpcError(c gnet.Conn, streamID uint32) {
	_ = workerPool.Submit(func() {
		var out bytes.Buffer
		fr := http2.NewFramer(&out, nil)
		var hbuf bytes.Buffer
		encoder := hpack.NewEncoder(&hbuf)
		encoder.WriteField(hpack.HeaderField{Name: "grpc-status", Value: "13"})
		encoder.WriteField(hpack.HeaderField{Name: "grpc-message", Value: "Internal error"})
		fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      streamID,
			EndHeaders:    true,
			EndStream:     true,
			BlockFragment: hbuf.Bytes(),
		})
		c.AsyncWrite(out.Bytes(), nil)
	})
}
