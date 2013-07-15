// Copyright 2013 Alexandre Fiori
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// FreeSWITCH Event Socket library for the Go programming language.
//
// eventsocket supports both inbound and outbound event socket connections,
// acting either as a client connecting to FreeSWITCH or as a server accepting
// connections from FreeSWITCH to control calls.
//
// Reference:
// http://wiki.freeswitch.org/wiki/Event_Socket
// http://wiki.freeswitch.org/wiki/Event_Socket_Outbound
//
// WORK IN PROGRESS, USE AT YOUR OWN RISK.
package eventsocket

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const BufferSize = 1024 << 6

var errMissingAuthRequest = errors.New("Missing auth request")
var errInvalidPassword = errors.New("Invalid password")
var errInvalidCommand = errors.New("Invalid command contains \\r or \\n")

// Handler is the event socket connection handler.
type Handler struct {
	conn       net.Conn
	reader     *bufio.Reader
	textreader *textproto.Reader
}

// newHandler allocates a new Handler and initialize its buffers.
func newHandler(c net.Conn) *Handler {
	h := Handler{conn: c, reader: bufio.NewReaderSize(c, BufferSize)}
	h.textreader = textproto.NewReader(h.reader)
	return &h
}

// HandleFunc is the function called on new incoming connections.
type HandleFunc func(*Handler)

// ListenAndServe listens for incoming connections from FreeSWITCH and calls
// HandleFunc in a new goroutine for each client.
//
// Example:
//
//	func main() {
//		eventsocket.ListenAndServe(":9090", handler)
//	}
//
//	func handler(c *eventsocket.Handler) {
//		ev, err := c.Send("connect") // must always start with this
//		ev.PrettyPrint()             // print event to the console
//		...
//		c.Send("myevents")
//		for {
//			ev, err = c.ReadEvent()
//			...
//		}
//	}
//
func ListenAndServe(addr string, fn HandleFunc) error {
	srv, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for {
		c, err := srv.Accept()
		if err != nil {
			return err
		}
		go fn(newHandler(c))
	}
	return nil
}

// Dial attemps to connect to FreeSWITCH and authenticate.
//
// Example:
//
//	c, _ := eventsocket.Dial("localhost:8021", "ClueCon")
//	ev, _ := c.Send("events plain ALL") // or events json ALL
//	for {
//		ev, _ = c.ReadEvent()
//		ev.PrettyPrint()
//		...
//	}
//
func Dial(addr, passwd string) (*Handler, error) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	h := newHandler(c)
	m, err := h.textreader.ReadMIMEHeader()
	if err != nil {
		c.Close()
		return nil, err
	}
	if m.Get("Content-Type") != "auth/request" {
		c.Close()
		return nil, errMissingAuthRequest
	}
	fmt.Fprintf(c, "auth %s\r\n\r\n", passwd)
	m, err = h.textreader.ReadMIMEHeader()
	if err != nil {
		c.Close()
		return nil, err
	}
	if m.Get("Reply-Text") != "+OK accepted" {
		c.Close()
		return nil, errInvalidPassword
	}
	return h, err
}

// RemoteAddr returns the remote addr of the connection.
func (h *Handler) RemoteAddr() net.Addr {
	return h.conn.RemoteAddr()
}

// Close terminates the connection.
func (h *Handler) Close() {
	h.conn.Close()
}

// ReadEvent reads and returns events from the server. It supports both plain
// or json, but *not* XML.
//
// When subscribing to events (e.g. `Send("events json ALL")`) it makes no
// difference to use plain or json. ReadEvent will parse them and return
// all headers and the body (if any) in an Event struct.
func (h *Handler) ReadEvent() (*Event, error) {
	hdr, err := h.textreader.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	resp := new(Event)
	resp.Header = make(EventHeader)
	if v := hdr.Get("Content-Length"); v != "" {
		length, err := strconv.Atoi(v)
		if err != nil {
			return nil, err
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(h.reader, b); err != nil {
			return nil, err
		}
		resp.Body = string(b)
	}
	switch hdr.Get("Content-Type") {
	case "command/reply":
		reply := hdr.Get("Reply-Text")
		if reply[:2] == "-E" {
			return nil, errors.New(reply[5:])
		}
		copyHeaders(&hdr, resp, false)
	case "api/response":
		if string(resp.Body[:2]) == "-E" {
			return nil, errors.New(string(resp.Body)[5:])
		}
		copyHeaders(&hdr, resp, false)
	case "text/event-plain":
		reader := bufio.NewReader(bytes.NewReader([]byte(resp.Body)))
		resp.Body = ""
		textreader := textproto.NewReader(reader)
		hdr, err = textreader.ReadMIMEHeader()
		if err != nil {
			return nil, err
		}
		if v := hdr.Get("Content-Length"); v != "" {
			length, err := strconv.Atoi(v)
			if err != nil {
				return nil, err
			}
			b := make([]byte, length)
			if _, err = io.ReadFull(reader, b); err != nil {
				return nil, err
			}
			resp.Body = string(b)
		}
		copyHeaders(&hdr, resp, true)
	case "text/event-json":
		err := json.Unmarshal([]byte(resp.Body), &resp.Header)
		if err != nil {
			return nil, err
		}
		if v, _ := resp.Header["_body"]; v != "" {
			resp.Body = v
			delete(resp.Header, "_body")
		} else {
			resp.Body = ""
		}
	}
	return resp, nil
}

// copyHeaders copies all keys and values from the MIMEHeader to Event.Header,
// normalizing (unescaping) its values.
//
// It's used after parsing plain text event headers, but not JSON.
func copyHeaders(src *textproto.MIMEHeader, dst *Event, decode bool) {
	var err error
	for k, v := range *src {
		if decode {
			dst.Header[k], err = url.QueryUnescape(v[0])
			if err != nil {
				dst.Header[k] = v[0]
			}
		} else {
			dst.Header[k] = v[0]
		}
	}
}

// Send sends a single command to the server and returns a response Event.
//
// See http://wiki.freeswitch.org/wiki/Event_Socket#Command_Documentation for
// details.
func (h *Handler) Send(command string) (*Event, error) {
	// Sanity check to avoid breaking the parser
	if strings.IndexAny(command, "\r\n") > 0 {
		return nil, errInvalidCommand
	}
	fmt.Fprintf(h.conn, "%s\r\n\r\n", command)
	return h.ReadEvent()
}

// MSG is the container used by SendMsg to store messages sent to FreeSWITCH.
// It's supposed to be populated with directives supported by the sendmsg
// command only, like "call-command: execute".
//
// See http://wiki.freeswitch.org/wiki/Event_Socket#sendmsg for details.
type MSG map[string]string

// SendMsg sends messages to FreeSWITCH and returns a response Event.
//
// Examples:
//
//	SendMsg(MSG{
//		"call-command": "hangup",
//		"hangup-cause": "we're done!",
//	}, "", "")
//
//	SendMsg(MSG{
//		"call-command":     "execute",
//		"execute-app-name": "playback",
//		"execute-app-arg":  "/tmp/test.wav",
//	}, "", "")
//
// Keys with empty values are ignored; uuid and appData are optional.
// If appData is set, a "content-length" header is expected (lower case!).
//
// See http://wiki.freeswitch.org/wiki/Event_Socket#sendmsg for details.
func (h *Handler) SendMsg(m MSG, uuid, appData string) (*Event, error) {
	b := bytes.NewBufferString("sendmsg")
	if uuid != "" {
		// Make sure there's no \r or \n in the UUID.
		if strings.IndexAny(uuid, "\r\n") > 0 {
			return nil, errInvalidCommand
		}
		b.WriteString(" " + uuid)
	}
	b.WriteString("\n")
	for k, v := range m {
		// Make sure there's no \r or \n in the key, and value.
		if strings.IndexAny(k, "\r\n") > 0 {
			return nil, errInvalidCommand
		}
		if v != "" {
			if strings.IndexAny(v, "\r\n") > 0 {
				return nil, errInvalidCommand
			}
			b.WriteString(fmt.Sprintf("%s: %s\n", k, v))
		}
	}
	b.WriteString("\n")
	if m["content-length"] != "" && appData != "" {
		b.WriteString(appData)
	}
	if _, err := b.WriteTo(h.conn); err != nil {
		return nil, err
	}
	return h.ReadEvent()
}

// Execute is a shortcut to SendMsg with call-command: execute without UUID,
// suitable for use on outbound event socket connections (acting as server).
//
// Example:
//
//	Execute("playback", "/tmp/test.wav", false)
//
// See http://wiki.freeswitch.org/wiki/Event_Socket#execute for details.
func (h *Handler) Execute(appName, appArg string, lock bool) (*Event, error) {
	var evlock string
	if lock {
		// Could be strconv.FormatBool(lock), but we don't want to
		// send event-lock when it's set to false.
		evlock = "true"
	}
	return h.SendMsg(MSG{
		"call-command":     "execute",
		"execute-app-name": appName,
		"execute-app-arg":  appArg,
		"event-lock":       evlock,
	}, "", "")
}

// ExecuteUUID is similar to Execute, but takes a UUID and no lock. Suitable
// for use on inbound event socket connections (acting as client).
func (h *Handler) ExecuteUUID(uuid, appName, appArg string) (*Event, error) {
	return h.SendMsg(MSG{
		"call-command":     "execute",
		"execute-app-name": appName,
		"execute-app-arg":  appArg,
	}, uuid, "")
}

// EventHeader represents events as a pair of key:value.
type EventHeader map[string]string

// Event represents a FreeSWITCH event.
type Event struct {
	Header EventHeader // Event headers, key:val
	Body   string      // Raw body, available in some events
}

func (r *Event) String() string {
	if r.Body == "" {
		return fmt.Sprintf("%s", r.Header)
	} else {
		return fmt.Sprintf("%s body=%s", r.Header, r.Body)
	}
}

// Get returns an Event value, or "" if the key doesn't exist.
func (r *Event) Get(key string) string {
	return r.Header[key]
}

// GetInt returns an Event value converted to int, or an error if conversion
// is not possible.
func (r *Event) GetInt(key string) (int, error) {
	n, err := strconv.Atoi(r.Header[key])
	if err != nil {
		return 0, err
	}
	return n, nil
}

// PrettyPrint prints Event headers and body to the standard output.
func (r *Event) PrettyPrint() {
	var keys []string
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s: %s\n", k, r.Header[k])
	}
	if r.Body != "" {
		fmt.Printf("BODY: %#v\n", r.Body)
	}
}
