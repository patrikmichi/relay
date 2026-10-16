package tunnel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ControlSocket is the name of the control socket in the state directory.
const ControlSocket = "control.sock"

// maxSocketPath is the longest unix socket path portable systems accept.
const maxSocketPath = 104

type controlRequest struct {
	Cmd string `json:"cmd"`
}

type controlReply struct {
	OK     bool    `json:"ok"`
	Error  string  `json:"error,omitempty"`
	Status *Status `json:"status,omitempty"`
}

// ServeControl answers local commands on <state-dir>/control.sock until ctx
// ends: status, pause, resume, rotate and reload. The socket is owned by the
// agent's user and readable by no one else.
func (a *Agent) ServeControl(ctx context.Context) error {
	path := filepath.Join(a.stateDir(), ControlSocket)
	if len(path) > maxSocketPath {
		return fmt.Errorf("the control socket path %s is longer than %d characters; use a shorter state_dir", path, maxSocketPath)
	}
	if err := os.MkdirAll(a.stateDir(), 0o750); err != nil {
		return err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		_ = os.Remove(path)
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go a.handleControl(c)
	}
}

func (a *Agent) handleControl(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	var req controlRequest
	line, err := bufio.NewReader(io.LimitReader(c, 4096)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return
	}
	reply := controlReply{OK: true}
	if json.Unmarshal(line, &req) != nil {
		reply = controlReply{Error: "the request is not JSON"}
	} else {
		switch req.Cmd {
		case "status":
		case "pause":
			a.Pause()
		case "resume":
			a.Resume()
		case "rotate":
			a.RotateNow()
		case "reload":
			a.Reload()
		default:
			reply = controlReply{Error: fmt.Sprintf("unknown command %q", req.Cmd)}
		}
	}
	if reply.OK {
		st := a.Status()
		reply.Status = &st
	}
	raw, _ := json.Marshal(reply)
	_, _ = c.Write(append(raw, '\n'))
}

// ControlRequest sends one command to a running agent.
func ControlRequest(stateDir, cmd string) (*Status, error) {
	path := filepath.Join(stateDir, ControlSocket)
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the agent is not running here, or %s cannot be opened by this user: %w", path, err)
	}
	defer c.Close()
	return controlExchange(c, cmd)
}

// controlExchange sends one command over an open connection and reads the
// agent's answer.
func controlExchange(c net.Conn, cmd string) (*Status, error) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	raw, _ := json.Marshal(controlRequest{Cmd: cmd})
	if _, err := c.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var reply controlReply
	if err := json.Unmarshal(line, &reply); err != nil {
		return nil, fmt.Errorf("the agent's answer is not JSON: %w", err)
	}
	if !reply.OK {
		return nil, errors.New(reply.Error)
	}
	return reply.Status, nil
}
