package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/fcgi"
	"os"
	"unsafe"
)

// #include "videostreamer.h"
// #include <stdlib.h>
// #cgo LDFLAGS: -lavformat -lavdevice -lavcodec -lavutil
// #cgo CFLAGS: -std=c11
// #cgo pkg-config: libavcodec
import "C"

// Args holds command line arguments.
type Args struct {
	ListenHost  string
	ListenPort  int
	InputFormat string
	InputURL    string
	Verbose     bool
	// Serve with FCGI protocol (true) or HTTP (false).
	FCGI bool
}

// HTTPHandler allows us to pass information to our request handlers.
type HTTPHandler struct {
	Verbose    bool
	ClientChan chan<- *Client
}

// Client is servicing one HTTP client.
type Client struct {
	// packetWriter goroutine writes out video packets to this pipe. HTTP
	// goroutine reads from the read side.
	OutPipe *os.File

	// Reference to a media output context. Through this, the packetWriter
	// goroutine writes packets to the write side of the pipe.
	Output *C.struct_VSOutput

	// Encoder writes packets to this channel, then the packetWriter goroutine
	// writes them to the pipe.
	PacketChan chan *C.AVPacket
}

func main() {
	args, err := getArgs()
	if err != nil {
		log.Fatalf("Invalid argument: %s", err)
	}

	C.vs_setup()

	// Clients provide encoder info about themselves when they start up.
	clientChan := make(chan *Client)

	go encoder(args.InputFormat, args.InputURL, args.Verbose, clientChan)

	// Start serving either with HTTP or FastCGI.

	hostPort := fmt.Sprintf("%s:%d", args.ListenHost, args.ListenPort)

	handler := HTTPHandler{
		Verbose:    args.Verbose,
		ClientChan: clientChan,
	}

	if args.FCGI {
		listener, err := net.Listen("tcp", hostPort)
		if err != nil {
			log.Fatalf("Unable to listen: %s", err)
		}

		log.Printf("Starting to serve requests on %s (FastCGI)", hostPort)

		err = fcgi.Serve(listener, handler)
		if err != nil {
			log.Fatalf("Unable to serve: %s", err)
		}
	} else {
		s := &http.Server{
			Addr:    hostPort,
			Handler: handler,
		}

		log.Printf("Starting to serve requests on %s (HTTP)", hostPort)

		err = s.ListenAndServe()
		if err != nil {
			log.Fatalf("Unable to serve: %s", err)
		}
	}
}

// getArgs retrieves and validates command line arguments.
func getArgs() (Args, error) {
	listenHost := flag.String("host", "0.0.0.0", "Host to listen on.")
	listenPort := flag.Int("port", 8080, "Port to listen on.")
	format := flag.String("format", "pulse", "Input format. Example: rtsp for RTSP.")
	input := flag.String("input", "", "Input URL valid for the given format. For RTSP you can provide a rtsp:// URL.")
	verbose := flag.Bool("verbose", false, "Enable verbose logging output.")
	fcgi := flag.Bool("fcgi", true, "Serve using FastCGI (true) or as a regular HTTP server.")

	flag.Parse()

	if len(*listenHost) == 0 {
		flag.PrintDefaults()
		return Args{}, fmt.Errorf("you must provide a host")
	}

	if len(*format) == 0 {
		flag.PrintDefaults()
		return Args{}, fmt.Errorf("you must provide an input format")
	}

	if len(*input) == 0 {
		flag.PrintDefaults()
		return Args{}, fmt.Errorf("you must provide an input URL")
	}

	return Args{
		ListenHost:  *listenHost,
		ListenPort:  *listenPort,
		InputFormat: *format,
		InputURL:    *input,
		Verbose:     *verbose,
		FCGI:        *fcgi,
	}, nil
}

func encoder(inputFormat, inputURL string, verbose bool,
	clientChan <-chan *Client) {
	clients := []*Client{}
	var input *C.struct_VSInput

	for {
		// If there are no clients, then block waiting for one.
		if len(clients) == 0 {
			client := <-clientChan
			log.Printf("encoder: New client")
			clients = append(clients, client)
			continue
		}

		// There is at least one client.

		// Get any new clients, but don't block.
		clients = acceptClients(clientChan, clients)

		// Open the input if it is not open yet.
		if input == nil {
			input = openInput(inputFormat, inputURL, verbose)
			if input == nil {
				log.Printf("encoder: Unable to open input")
				cleanupClients(clients)
				return
			}

			if verbose {
				log.Printf("encoder: Opened input")
			}
		}

		// Read a packet.
		var pkt C.AVPacket
		readRes := C.int(0)
		readRes = C.vs_read_packet(input, &pkt, C.bool(verbose))
		if readRes == -1 {
			log.Printf("encoder: Failure reading packet")
			C.vs_destroy_input(input)
			cleanupClients(clients)
			return
		}

		if readRes == 0 {
			continue
		}

		// Write the packet to all clients.
		clients = writePacketToClients(input, &pkt, clients, verbose)

		C.av_packet_unref(&pkt)

		// If we get down to zero clients, close the input.
		if len(clients) == 0 {
			C.vs_destroy_input(input)
			input = nil
		}
	}
}

func acceptClients(clientChan <-chan *Client, clients []*Client) []*Client {
	for {
		select {
		case client := <-clientChan:
			clients = append(clients, client)
		default:
			return clients
		}
	}
}

func cleanupClients(clients []*Client) {
	for _, client := range clients {
		cleanupClient(client)
	}
}

func cleanupClient(client *Client) {
	// Closing write side will make read side receive EOF.
	if client.OutPipe != nil {
		_ = client.OutPipe.Close()
		client.OutPipe = nil
	}

	if client.Output != nil {
		C.vs_destroy_output(client.Output)
		client.Output = nil
	}

	if client.PacketChan != nil {
		close(client.PacketChan)

		// Drain it. The packetWriter should be draining it too. However it is
		// possible that it ended.
		//
		// Note one may think that draining both here and in the packetWriter could
		// lead to the unfortunate likelihood that the client will receive some
		// packets but not others, leading to corruption. But since we closed the
		// write side of the pipe above, this will not happen. No further packets
		// will be reaching the client.
		for pkt := range client.PacketChan {
			C.av_packet_free(&pkt)
		}

		client.PacketChan = nil
	}
}

func openInput(inputFormat, inputURL string, verbose bool) *C.struct_VSInput {
	inputFormatC := C.CString(inputFormat)
	inputURLC := C.CString(inputURL)

	input := C.vs_open_input(inputFormatC, inputURLC, C.bool(verbose))
	if input == nil {
		log.Printf("Unable to open input")
		C.free(unsafe.Pointer(inputFormatC))
		C.free(unsafe.Pointer(inputURLC))
		return nil
	}
	C.free(unsafe.Pointer(inputFormatC))
	C.free(unsafe.Pointer(inputURLC))

	return input
}

// Try to write the packet to each client. If we fail, we clean up the client
// and it will not be in the returned list of clients.
func writePacketToClients(input *C.struct_VSInput, pkt *C.AVPacket,
	clients []*Client, verbose bool) []*Client {
	// Rewrite clients slice with only those we succeeded in writing to. If we
	// failed for some reason we clean up the client and no longer send it
	// anything further.
	clients2 := []*Client{}

	for _, client := range clients {
		// Open the client's output if it is not yet open.
		if client.Output == nil {
			outputFormat := "mp4"
			outputURL := fmt.Sprintf("pipe:%d", client.OutPipe.Fd())
			client.Output = openOutput(outputFormat, outputURL, verbose, input)
			if client.Output == nil {
				log.Printf("Unable to open output for client")
				cleanupClient(client)
				continue
			}

			// We pass packets to the client via this channel. We give each client
			// its own goroutine for the purposes of receiving these packets and
			// writing them to the write side of the pipe. We do it this way rather
			// than directly here because we do not want the encoder to block waiting
			// on a write to the write side of the pipe because there is a slow HTTP
			// client.
			client.PacketChan = make(chan *C.AVPacket, 32)

			go packetWriter(client, input, verbose)

			log.Printf("Opened output for client")
		}

		// Duplicate the packet. Each client's goroutine will receive a copy.
		pktCopy := C.av_packet_clone(pkt)
		if pktCopy == nil {
			log.Printf("Unable to clone packet")
			cleanupClient(client)
			continue
		}

		// Pass the client to a goroutine that writes it to this client.
		select {
		case client.PacketChan <- pktCopy:
		default:
			log.Printf("Client too slow")
			C.av_packet_free(&pktCopy)
			cleanupClient(client)
			continue
		}

		// Successful so far. Keep the client around.
		clients2 = append(clients2, client)
	}

	return clients2
}

// Receive packets from the encoder, and write them out to the client's pipe.
//
// We end when encoder closes the channel, or if we encounter a write error.
func packetWriter(client *Client, input *C.struct_VSInput, verbose bool) {
	for pkt := range client.PacketChan {
		writeRes := C.int(0)
		writeRes = C.vs_write_packet(input, client.Output, pkt, C.bool(verbose))
		if writeRes == -1 {
			log.Printf("Failure writing packet")
			C.av_packet_free(&pkt)
			return
		}
		C.av_packet_free(&pkt)
	}
}

// Open the output file. This creates an MP4 container and writes the header to
// the given output URL.
func openOutput(outputFormat, outputURL string, verbose bool,
	input *C.struct_VSInput) *C.struct_VSOutput {
	outputFormatC := C.CString("mp4")
	outputURLC := C.CString(outputURL)

	output := C.vs_open_output(outputFormatC, outputURLC, input, C.bool(verbose))
	if output == nil {
		log.Printf("Unable to open output")
		C.free(unsafe.Pointer(outputFormatC))
		C.free(unsafe.Pointer(outputURLC))
		return nil
	}
	C.free(unsafe.Pointer(outputFormatC))
	C.free(unsafe.Pointer(outputURLC))

	return output
}

// ServeHTTP handles an HTTP request.
func (h HTTPHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	log.Printf("Serving [%s] request from [%s] to path [%s] (%d bytes)",
		r.Method, r.RemoteAddr, r.URL.Path, r.ContentLength)

	if r.Method == "GET" && r.URL.Path == "/stream" {
		h.streamRequest(rw, r)
		return
	}

	log.Printf("Unknown request.")
	rw.WriteHeader(http.StatusNotFound)
	_, _ = rw.Write([]byte("<h1>404 Not found</h1>"))
}

// Read from a pipe where streaming media shows up. We read a chunk and write it
// immediately to the client, and repeat forever (until either the client goes
// away, or an error of some kind occurs).
func (h HTTPHandler) streamRequest(rw http.ResponseWriter, r *http.Request) {
	// The encoder writes to the out pipe (using the packetWriter goroutine). We
	// read from the in pipe.
	inPipe, outPipe, err := os.Pipe()
	if err != nil {
		log.Printf("Unable to open pipe: %s", err)
		rw.WriteHeader(http.StatusInternalServerError)
		_, _ = rw.Write([]byte("<h1>500 Internal server error</h1>"))
		return
	}

	c := &Client{
		OutPipe: outPipe,
	}

	// Tell the encoder we're here.
	h.ClientChan <- c

	rw.Header().Set("Content-Type", "video/mp4")
	rw.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	// We send chunked by default

	for {
		buf := make([]byte, 1024)
		readSize, err := inPipe.Read(buf)
		if err != nil {
			log.Printf("%s: Read error: %s", r.RemoteAddr, err)
			break
		}

		// We get EOF if write side of pipe closed.
		if readSize == 0 {
			log.Printf("%s: EOF", r.RemoteAddr)
			break
		}

		writeSize, err := rw.Write(buf[:readSize])
		if err != nil {
			log.Printf("%s: Write error: %s", r.RemoteAddr, err)
			break
		}

		if writeSize != readSize {
			log.Printf("%s: Short write", r.RemoteAddr)
			break
		}

		// ResponseWriter buffers chunks. Flush them out ASAP to reduce the time a
		// client is waiting, especially initially.
		if flusher, ok := rw.(http.Flusher); ok {
			flusher.Flush()
		}

		if h.Verbose {
			//log.Printf("%s: Sent %d bytes to client", r.RemoteAddr, n)
		}
	}

	// Writes to write side will raise error when read side is closed.
	_ = inPipe.Close()

	log.Printf("%s: Client cleaned up", r.RemoteAddr)
}
