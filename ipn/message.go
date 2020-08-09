// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ipn

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"golang.org/x/oauth2"
	"tailscale.com/types/logger"
	"tailscale.com/types/structs"
	"tailscale.com/version"
)

type NoArgs struct{}

type StartArgs struct {
	Opts Options
}

type SetPrefsArgs struct {
	New *Prefs
}

type FakeExpireAfterArgs struct {
	Duration time.Duration
}

type PingArgs struct {
	IP string
}

// Command is a command message that is JSON encoded and sent by a
// frontend to a backend.
type Command struct {
	_ structs.Incomparable

	// Version is the binary version of the frontend (the client).
	Version string

	// AllowVersionSkew controls whether it's permitted for the
	// client and server to have a different version. The default
	// (false) means to be strict.
	AllowVersionSkew bool

	// Exactly one of the following must be non-nil.
	Quit                  *NoArgs
	Start                 *StartArgs
	StartLoginInteractive *NoArgs
	Login                 *oauth2.Token
	Logout                *NoArgs
	SetPrefs              *SetPrefsArgs
	RequestEngineStatus   *NoArgs
	RequestStatus         *NoArgs
	FakeExpireAfter       *FakeExpireAfterArgs
	Ping                  *PingArgs
}

type BackendServer struct {
	logf          logger.Logf
	b             Backend              // the Backend we are serving up
	sendNotifyMsg func(jsonMsg []byte) // send a notification message
	GotQuit       bool                 // a Quit command was received
}

func NewBackendServer(logf logger.Logf, b Backend, sendNotifyMsg func(b []byte)) *BackendServer {
	return &BackendServer{
		logf:          logf,
		b:             b,
		sendNotifyMsg: sendNotifyMsg,
	}
}

func (bs *BackendServer) send(n Notify) {
	n.Version = version.LONG
	b, err := json.Marshal(n)
	if err != nil {
		log.Fatalf("Failed json.Marshal(notify): %v\n%#v", err, n)
	}
	bs.sendNotifyMsg(b)
}

func (bs *BackendServer) SendErrorMessage(msg string) {
	bs.send(Notify{ErrMessage: &msg})
}

// GotCommandMsg parses the incoming message b as a JSON Command and
// calls GotCommand with it.
func (bs *BackendServer) GotCommandMsg(b []byte) error {
	cmd := &Command{}
	if len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, cmd); err != nil {
		return err
	}
	return bs.GotCommand(cmd)
}

func (bs *BackendServer) GotFakeCommand(cmd *Command) error {
	cmd.Version = version.LONG
	return bs.GotCommand(cmd)
}

func (bs *BackendServer) GotCommand(cmd *Command) error {
	if cmd.Version != version.LONG && !cmd.AllowVersionSkew {
		vs := fmt.Sprintf("GotCommand: Version mismatch! frontend=%#v backend=%#v",
			cmd.Version, version.LONG)
		bs.logf("%s", vs)
		// ignore the command, but send a message back to the
		// caller so it can realize the version mismatch too.
		// We don't want to exit because it might cause a crash
		// loop, and restarting won't fix the problem.
		bs.send(Notify{
			ErrMessage: &vs,
		})
		return nil
	}
	if cmd.Quit != nil {
		bs.GotQuit = true
		return errors.New("Quit command received")
	}

	if c := cmd.Start; c != nil {
		opts := c.Opts
		opts.Notify = bs.send
		return bs.b.Start(opts)
	} else if c := cmd.StartLoginInteractive; c != nil {
		bs.b.StartLoginInteractive()
		return nil
	} else if c := cmd.Login; c != nil {
		bs.b.Login(c)
		return nil
	} else if c := cmd.Logout; c != nil {
		bs.b.Logout()
		return nil
	} else if c := cmd.SetPrefs; c != nil {
		bs.b.SetPrefs(c.New)
		return nil
	} else if c := cmd.RequestEngineStatus; c != nil {
		bs.b.RequestEngineStatus()
		return nil
	} else if c := cmd.RequestStatus; c != nil {
		bs.b.RequestStatus()
		return nil
	} else if c := cmd.FakeExpireAfter; c != nil {
		bs.b.FakeExpireAfter(c.Duration)
		return nil
	} else if c := cmd.Ping; c != nil {
		bs.b.Ping(c.IP)
		return nil
	} else {
		return fmt.Errorf("BackendServer.Do: no command specified")
	}
}

func (bs *BackendServer) Reset() error {
	// Tell the backend we got a Logout command, which will cause it
	// to forget all its authentication information.
	return bs.GotFakeCommand(&Command{Logout: &NoArgs{}})
}

type BackendClient struct {
	logf           logger.Logf
	sendCommandMsg func(jsonb []byte)
	notify         func(Notify)

	// AllowVersionSkew controls whether to allow mismatched
	// frontend & backend versions.
	AllowVersionSkew bool
}

func NewBackendClient(logf logger.Logf, sendCommandMsg func(jsonb []byte)) *BackendClient {
	return &BackendClient{
		logf:           logf,
		sendCommandMsg: sendCommandMsg,
	}
}

func (bc *BackendClient) GotNotifyMsg(b []byte) {
	if len(b) == 0 {
		// not interesting
		return
	}
	n := Notify{}
	if err := json.Unmarshal(b, &n); err != nil {
		log.Fatalf("BackendClient.Notify: cannot decode message (length=%d)\n%#v", len(b), string(b))
	}
	if n.Version != version.LONG && !bc.AllowVersionSkew {
		vs := fmt.Sprintf("GotNotify: Version mismatch! frontend=%#v backend=%#v",
			version.LONG, n.Version)
		bc.logf("%s", vs)
		// delete anything in the notification except the version,
		// to prevent incorrect operation.
		n = Notify{
			Version:    n.Version,
			ErrMessage: &vs,
		}
	}
	if bc.notify != nil {
		bc.notify(n)
	}
}

func (bc *BackendClient) send(cmd Command) {
	cmd.Version = version.LONG
	b, err := json.Marshal(cmd)
	if err != nil {
		log.Fatalf("Failed json.Marshal(cmd): %v\n%#v\n", err, cmd)
	}
	bc.sendCommandMsg(b)
}

func (bc *BackendClient) SetNotifyCallback(fn func(Notify)) {
	bc.notify = fn
}

func (bc *BackendClient) Quit() error {
	bc.send(Command{Quit: &NoArgs{}})
	return nil
}

func (bc *BackendClient) Start(opts Options) error {
	bc.notify = opts.Notify
	opts.Notify = nil // server can't call our function pointer
	bc.send(Command{Start: &StartArgs{Opts: opts}})
	return nil // remote Start() errors must be handled remotely
}

func (bc *BackendClient) StartLoginInteractive() {
	bc.send(Command{StartLoginInteractive: &NoArgs{}})
}

func (bc *BackendClient) Login(token *oauth2.Token) {
	bc.send(Command{Login: token})
}

func (bc *BackendClient) Logout() {
	bc.send(Command{Logout: &NoArgs{}})
}

func (bc *BackendClient) SetPrefs(new *Prefs) {
	bc.send(Command{SetPrefs: &SetPrefsArgs{New: new}})
}

func (bc *BackendClient) RequestEngineStatus() {
	bc.send(Command{RequestEngineStatus: &NoArgs{}})
}

func (bc *BackendClient) RequestStatus() {
	bc.send(Command{AllowVersionSkew: true, RequestStatus: &NoArgs{}})
}

func (bc *BackendClient) FakeExpireAfter(x time.Duration) {
	bc.send(Command{FakeExpireAfter: &FakeExpireAfterArgs{Duration: x}})
}

func (bc *BackendClient) Ping(ip string) {
	bc.send(Command{Ping: &PingArgs{IP: ip}})
}

// MaxMessageSize is the maximum message size, in bytes.
const MaxMessageSize = 10 << 20

// TODO(apenwarr): incremental json decode?
//  That would let us avoid storing the whole byte array uselessly in RAM.
func ReadMsg(r io.Reader) ([]byte, error) {
	cb := make([]byte, 4)
	_, err := io.ReadFull(r, cb)
	if err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(cb)
	if n > MaxMessageSize {
		return nil, fmt.Errorf("ipn.Read: message too large: %v bytes", n)
	}
	b := make([]byte, n)
	nn, err := io.ReadFull(r, b)
	if err != nil {
		return nil, err
	}
	if nn != int(n) {
		return nil, fmt.Errorf("ipn.Read: expected %v bytes, got %v", n, nn)
	}
	return b, nil
}

// TODO(apenwarr): incremental json encode?
//  That would save RAM, at the expense of having to encode once so that
//  we can produce the initial byte count.
func WriteMsg(w io.Writer, b []byte) error {
	// TODO(bradfitz): this does two writes to w, which likely
	// does two writes on the wire, two frame generations, etc. We
	// should take a concrete buffered type, or use a sync.Pool to
	// allocate a buf and do one write.
	cb := make([]byte, 4)
	if len(b) > MaxMessageSize {
		return fmt.Errorf("ipn.Write: message too large: %v bytes", len(b))
	}
	binary.LittleEndian.PutUint32(cb, uint32(len(b)))
	n, err := w.Write(cb)
	if err != nil {
		return err
	}
	if n != 4 {
		return fmt.Errorf("ipn.Write: short write: %v bytes (wanted 4)", n)
	}
	n, err = w.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return fmt.Errorf("ipn.Write: short write: %v bytes (wanted %v)", n, len(b))
	}
	return nil
}
