/*
* Honeytrap
* Copyright (C) 2016-2017 DutchSec (https://dutchsec.com/)
*
* This program is free software; you can redistribute it and/or modify it under
* the terms of the GNU Affero General Public License version 3 as published by the
* Free Software Foundation.
*
* This program is distributed in the hope that it will be useful, but WITHOUT
* ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
* FOR A PARTICULAR PURPOSE.  See the GNU Affero General Public License for more
* details.
*
* You should have received a copy of the GNU Affero General Public License
* version 3 along with this program in the file "LICENSE".  If not, see
* <http://www.gnu.org/licenses/agpl-3.0.txt>.
*
* See https://honeytrap.io/ for more details. All requests should be sent to
* licensing@honeytrap.io
*
* The interactive user interfaces in modified source and object code versions
* of this program must display Appropriate Legal Notices, as required under
* Section 5 of the GNU Affero General Public License version 3.
*
* In accordance with Section 7(b) of the GNU Affero General Public License version 3,
* these Appropriate Legal Notices must retain the display of the "Powered by
* Honeytrap" logo and retain the original copyright notice. If the display of the
* logo is not reasonably feasible for technical reasons, the Appropriate Legal Notices
* must display the words "Powered by Honeytrap" and retain the original copyright notice.
 */
package agent

import (
	"context"
	"encoding"
	"fmt"
	"io"
	"net"
	"runtime"

	"github.com/fatih/color"
	"github.com/honeytrap/honeytrap/listener"
	"github.com/mimoo/disco/libdisco"

	logging "github.com/op/go-logging"
)

var log = logging.MustGetLogger("listeners/agent")

//  Register the listener
var (
	_ = listener.Register("agent", New)
)

type agentListener struct {
	agentConfig

	ch        chan net.Conn
	Addresses []net.Addr

	net.Listener
}

type agentConfig struct {
	Listen string `toml:"listen"`
}

// AddAddress will add the addresses to listen to
func (sc *agentListener) AddAddress(a net.Addr) {
	sc.Addresses = append(sc.Addresses, a)
}

// New will initialize the agent listener
func New(options ...func(listener.Listener) error) (listener.Listener, error) {
	ch := make(chan net.Conn)

	l := agentListener{
		agentConfig: agentConfig{},
		ch:          ch,
	}

	for _, option := range options {
		option(&l)
	}

	return &l, nil
}

func (sl *agentListener) serv(c *conn2) {
	defer func() {
		if err := recover(); err != nil {
			trace := make([]byte, 1024)
			count := runtime.Stack(trace, true)
			log.Errorf("Error: %s", err)
			log.Errorf("Stack of %d bytes: %s\n", count, string(trace))
			return
		}
	}()

	log.Debugf("Agent connecting from remote address: %s", c.RemoteAddr())

	version := ""
	shortCommitID := ""

	if p, err := c.receive(); err == io.EOF {
		return
	} else if err != nil {
		log.Errorf("Error receiving object: %s", err.Error())
		return
	} else if h, ok := p.(*Handshake); !ok {
		log.Errorf("Expected handshake from Agent")
		return
	} else {
		version = h.Version
		shortCommitID = h.ShortCommitID
	}

	c.send(HandshakeResponse{
		sl.Addresses,
	})

	fmt.Println("Agent connected (version=%s, commitid=%s)...", version, shortCommitID)
	defer fmt.Println("Agent disconnected...")

	out := make(chan interface{})

	conns := Connections{}

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()

		c.Close()

		go func() {
			// drain
			for _ = range out {
			}
		}()

		conns.Each(func(conn *agentConnection) {
			conn.Close()
		})

		close(out)
	}()

	go func() {
		for {
			select {
			case p := <-out:
				if bm, ok := p.(encoding.BinaryMarshaler); !ok {
					log.Errorf("Error marshalling object")
					return
				} else if err := c.send(bm); err != nil {
					log.Errorf("Error sending object: %s", err.Error())
					return
				}
			case <-ctx.Done():
				return
			}

		}
	}()

	for {
		o, err := c.receive()
		if err == io.EOF {
			return
		} else if err != nil {
			log.Errorf("Error receiving object: %s", err.Error())
			return
		}

		switch v := o.(type) {
		case *Hello:
			ac := &agentConnection{
				Laddr: v.Laddr,
				Raddr: v.Raddr,
				in:    make(chan []byte),
				out:   out,
			}

			conns.Add(ac)

			sl.ch <- ac
		case *ReadWrite:
			conn := conns.Get(v.Laddr, v.Raddr)
			if conn == nil {
				continue
			}

			conn.receive(v.Payload)
		case *EOF:
			conn := conns.Get(v.Laddr, v.Raddr)
			if conn == nil {
				continue
			}

			conns.Delete(conn)

			conn.Close()
		case *Ping:
			log.Debugf("Received ping from agent: %s", c.RemoteAddr())
		}
	}

	return
}

// Start the listener
func (sl *agentListener) Start(ctx context.Context) error {
	storage, err := Storage()
	if err != nil {
		return err
	}

	keyPair, err := storage.KeyPair()
	if err != nil {
		return err
	}

	fmt.Println(color.YellowString("Honeytrap Agent Server public key: %s", keyPair.ExportPublicKey()))

	serverConfig := libdisco.Config{
		HandshakePattern: libdisco.Noise_NK,
		KeyPair:          keyPair,
	}

	listen := ":1339"
	if sl.Listen != "" {
		listen = sl.Listen
	}

	listener, err := libdisco.Listen("tcp", listen, &serverConfig)
	if err != nil {
		fmt.Println(color.RedString("Error starting listener: %s", err.Error()))
		return err
	}

	log.Infof("Listener started: %s", listen)

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				log.Errorf("Error accepting connection: %s", err.Error())
				continue
			}

			go sl.serv(Conn2(c))
		}
	}()

	return nil
}

// Accept a new connection
func (sl *agentListener) Accept() (net.Conn, error) {
	c := <-sl.ch
	return c, nil
}
