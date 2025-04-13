# grpc-gnet-example

## Description

This project demonstrates the implementation of a gRPC server using the high-performance network library [gnet](https://github.com/panjf2000/gnet).

## Features

- HTTP/2 protocol implementation for gRPC without using the standard net/http library
- Processing gRPC requests through non-blocking I/O using gnet
- High performance and low resource consumption
- Prometheus metrics support
- Scaling across multiple CPU cores

## How It Works

The server implements a simple echo service defined in protobuf. When a client sends a message, the server responds by adding the prefix "Echo: " to the received message.

Main components:

- HTTP/2 preface handling
- Working with HTTP/2 frames (SETTINGS, HEADERS, DATA, etc.)
- HPACK encoding and decoding for headers
- gRPC stream processing
- Serialization and deserialization of protobuf messages

## Starting the Server

```bash
go run main.go --port=9080 --multicore=true --eventloops=4 --metrics-port=8081
```

### Launch Parameters

- `port` - port for the gRPC server (default 9080)
- `multicore` - use multiple CPU cores (default true)
- `eventloops` - number of event loops (default 4)
- `metrics-port` - port for Prometheus metrics (default 8081)

## Performance

This implementation provides high performance through:

- Using a buffer pool to reduce GC pressure
- Asynchronous I/O through gnet
- A goroutine pool for request processing

## Metrics

The server exports Prometheus metrics at `http://localhost:8081/`