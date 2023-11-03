// Package iap provides a client for the IAP tunneling protocol.
package iap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"nhooyr.io/websocket"
)

var _ net.Conn = (*Conn)(nil)

const (
	proxySubproto = "relay.tunnel.cloudproxy.app"
	proxyHost     = "tunnel.cloudproxy.app"
	proxyPath     = "/v4/connect"
	proxyOrigin   = "bot:iap-tunneler"
)

const (
	subprotoMaxFrameSize        = 16384
	subprotoTagSuccess   uint16 = 0x1
	subprotoTagData      uint16 = 0x4
	subprotoTagAck       uint16 = 0x7
)

func min[T int | uint](a, b T) T {
	if a < b {
		return a
	}
	return b
}

// copyNBuffer is like io.CopyN but stages through a given buffer like io.CopyBuffer.
func copyNBuffer(w io.Writer, r io.Reader, n int64, buf []byte) (int64, error) {
	return io.CopyBuffer(w, io.LimitReader(r, n), buf)
}

type Conn struct {
	conn      net.Conn
	connected bool
	sessionID []byte

	recvNbAcked   uint64
	recvNbUnacked uint64
	recvBuf       []byte
	recvReader    *io.PipeReader
	recvWriter    *io.PipeWriter

	sendNbAcked   uint64
	sendNbUnacked uint64
	sendNbCh      chan int
	sendBuf       []byte
	sendReader    *io.PipeReader
	sendWriter    *io.PipeWriter
}

func connectURL(dopts *dialOptions) string {
	query := url.Values{
		"zone":      []string{dopts.Zone},
		"region":    []string{dopts.Region},
		"project":   []string{dopts.Project},
		"port":      []string{dopts.Port},
		"network":   []string{dopts.Network},
		"interface": []string{dopts.Interface},
		"instance":  []string{dopts.Instance},
		"host":      []string{dopts.Host},
		"group":     []string{dopts.Group},
	}

	for key, value := range query {
		if value[0] == "" {
			query.Del(key)
		}
	}

	url := url.URL{
		Scheme:   "wss",
		Host:     proxyHost,
		Path:     proxyPath,
		RawQuery: query.Encode(),
	}

	return url.String()
}

// Dial connects to the IAP proxy and returns a Conn or error if the connection fails.
func Dial(ctx context.Context, opts ...DialOption) (*Conn, error) {
	dopts := &dialOptions{}
	dopts.collectOpts(opts)

	header := make(http.Header)
	header.Set("Origin", proxyOrigin)

	if dopts.TokenSource != nil {
		token, err := (*dopts.TokenSource).Token()
		if err != nil {
			return nil, err
		}

		header.Set("Authorization", fmt.Sprintf("%v %v", token.Type(), token.AccessToken))
	}

	wsOptions := websocket.DialOptions{
		HTTPHeader:      header,
		Subprotocols:    []string{proxySubproto},
		CompressionMode: websocket.CompressionDisabled,
	}
	if dopts.Compress {
		wsOptions.CompressionMode = websocket.CompressionContextTakeover
	}

	conn, _, err := websocket.Dial(ctx, connectURL(dopts), &wsOptions)
	if err != nil {
		return nil, err
	}

	netConn := websocket.NetConn(context.Background(), conn, websocket.MessageBinary)

	recvReader, recvWriter := io.Pipe()
	sendReader, sendWriter := io.Pipe()

	c := &Conn{
		conn: netConn,

		recvBuf:    make([]byte, subprotoMaxFrameSize),
		recvReader: recvReader,
		recvWriter: recvWriter,

		sendNbCh:   make(chan int),
		sendBuf:    make([]byte, subprotoMaxFrameSize),
		sendReader: sendReader,
		sendWriter: sendWriter,
	}

	go c.read()
	go c.write()

	return c, nil
}

// LocalAddr returns the local network address.
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote network address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline sets the read and write deadlines associated with the connection.
func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// SetReadDeadline sets the deadline for future Read calls.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline sets the deadline for future Write calls.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// Close closes the connection.
func (c *Conn) Close() error {
	close(c.sendNbCh)
	return c.conn.Close()
}

// Read reads data from the connection.
func (c *Conn) Read(buf []byte) (n int, err error) {
	return c.recvReader.Read(buf)
}

// Write writes data to the connection.
func (c *Conn) Write(buf []byte) (n int, err error) {
	c.sendNbCh <- len(buf)
	return c.sendWriter.Write(buf)
}

// Connected returns whether the connection is established.
func (c *Conn) Connected() bool {
	return c.connected
}

// SessionID returns the session ID of the connection. This is only valid after the connection is established.
func (c *Conn) SessionID() string {
	return string(c.sessionID)
}

// Sent returns the number of bytes sent and acked.
func (c *Conn) Sent() uint64 {
	return c.sendNbAcked
}

// Received returns the number of bytes received and acked.
func (c *Conn) Received() uint64 {
	return c.recvNbAcked
}

func (c *Conn) readSuccessFrame(r io.Reader) error {
	bytes := [4]byte{}
	if _, err := r.Read(bytes[:]); err != nil {
		return err
	}
	len := binary.BigEndian.Uint32(bytes[:])

	if len > subprotoMaxFrameSize {
		return &ProtocolError{"len exceeds subprotocol max data frame size"}
	}

	c.sessionID = make([]byte, len)
	if _, err := r.Read(c.sessionID); err != nil {
		return err
	}

	c.connected = true
	return nil
}

func (c *Conn) writeAck(nb uint64) error {
	// allocation fine, cold path
	buf := make([]byte, 10)

	binary.BigEndian.PutUint16(buf[0:2], subprotoTagAck)
	binary.BigEndian.PutUint64(buf[2:10], nb)

	_, err := c.conn.Write(buf)
	return err
}

func (c *Conn) readAckFrame(r io.Reader) error {
	bytes := [8]byte{}
	if _, err := r.Read(bytes[:]); err != nil {
		return err
	}

	// TODO: should we transmit?
	// since it's over TCP this seems redundant

	c.sendNbAcked = binary.BigEndian.Uint64(bytes[:])
	return nil
}

func (c *Conn) readDataFrame(r io.Reader) error {
	bytes := [4]byte{}
	if _, err := r.Read(bytes[:]); err != nil {
		return err
	}
	len := binary.BigEndian.Uint32(bytes[:])

	if len > subprotoMaxFrameSize {
		return &ProtocolError{"len exceeds subprotocol max data frame size"}
	}

	if _, err := copyNBuffer(c.recvWriter, r, int64(len), c.recvBuf); err != nil {
		return err
	}

	c.recvNbUnacked += uint64(len)
	return nil
}

func (c *Conn) readFrame() error {
	bytes := [2]byte{}
	if _, err := c.conn.Read(bytes[:]); err != nil {
		return err
	}
	tag := binary.BigEndian.Uint16(bytes[:])

	var err error

	switch tag {
	case subprotoTagSuccess:
		err = c.readSuccessFrame(c.conn)
	default:
		if !c.connected {
			return &ProtocolError{"expected success frame but not did receive one"}
		}

		switch tag {
		case subprotoTagAck:
			err = c.readAckFrame(c.conn)
		case subprotoTagData:
			err = c.readDataFrame(c.conn)

			// can the threshold be increased?
			if c.recvNbUnacked-c.recvNbAcked > 2*subprotoMaxFrameSize {
				if err := c.writeAck(c.recvNbUnacked); err != nil {
					return err
				}
				c.recvNbAcked = c.recvNbUnacked
			}
		default:
			// unknown tags should be ignored
			return nil
		}

	}

	return err
}

func (c *Conn) writeFrame() error {
	nb, ok := <-c.sendNbCh
	if !ok {
		// connection is closing
		return io.EOF
	}

	for nb > 0 {
		// clamp each write to max frame size
		writeNb := min(nb, subprotoMaxFrameSize)
		nb -= writeNb

		var buf bytes.Buffer

		binary.Write(&buf, binary.BigEndian, subprotoTagData)
		binary.Write(&buf, binary.BigEndian, uint32(writeNb))

		if _, err := copyNBuffer(&buf, c.sendReader, int64(writeNb), c.sendBuf); err != nil {
			return err
		}

		writtenNb, err := c.conn.Write(buf.Bytes())
		if err != nil {
			return err
		}

		c.sendNbUnacked += uint64(writtenNb)
	}

	return nil
}

func (c *Conn) read() {
	for {
		if err := c.readFrame(); err != nil {
			var closeError websocket.CloseError
			if errors.As(err, &closeError) {
				err = &CloseError{int(closeError.Code), closeError.Reason}
			}

			// close write side of pipes only
			c.sendWriter.CloseWithError(err)
			c.recvWriter.CloseWithError(err)

			break
		}
	}
}

func (c *Conn) write() {
	for {
		if err := c.writeFrame(); err != nil {
			var closeError websocket.CloseError
			if errors.As(err, &closeError) {
				err = &CloseError{int(closeError.Code), closeError.Reason}
			}

			// close write side of pipes only
			c.sendWriter.CloseWithError(err)
			c.recvWriter.CloseWithError(err)

			break
		}
	}
}
