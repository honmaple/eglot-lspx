package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/sourcegraph/jsonrpc2"
	"go.lsp.dev/protocol"
)

type DiagnosticCache struct {
	mu    sync.RWMutex
	cache map[protocol.DocumentURI]map[string][]protocol.Diagnostic
}

func (c *DiagnosticCache) Set(uri protocol.DocumentURI, source string, diagnostics []protocol.Diagnostic) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cache[uri] == nil {
		c.cache[uri] = make(map[string][]protocol.Diagnostic)
	}
	c.cache[uri][source] = diagnostics
}

func (c *DiagnosticCache) Get(uri protocol.DocumentURI) []protocol.Diagnostic {
	c.mu.RLock()
	defer c.mu.RUnlock()

	results := []protocol.Diagnostic{}
	for _, diags := range c.cache[uri] {
		results = append(results, diags...)
	}
	return results
}

func NewDiagnosticCache() *DiagnosticCache {
	return &DiagnosticCache{
		cache: make(map[protocol.DocumentURI]map[string][]protocol.Diagnostic),
	}
}

type ProcessServer struct {
	cmd    *exec.Cmd
	name   string
	stdin  io.ReadCloser
	stdout io.WriteCloser

	diags *DiagnosticCache

	conn      *jsonrpc2.Conn
	proxyConn *jsonrpc2.Conn
}

func (s *ProcessServer) Name() string {
	return s.name
}

func (s *ProcessServer) Read(p []byte) (int, error) {
	return s.stdin.Read(p)
}

func (s *ProcessServer) Write(p []byte) (int, error) {
	return s.stdout.Write(p)
}

func (s *ProcessServer) Close() error {
	return errors.Join(s.stdin.Close(), s.stdout.Close(), s.cmd.Process.Kill())
}

func (s *ProcessServer) Call(ctx context.Context, method string, params *json.RawMessage, result any) error {
	return s.conn.Call(ctx, method, params, result)
}

func (s *ProcessServer) Notify(ctx context.Context, method string, params *json.RawMessage) error {
	return s.conn.Notify(ctx, method, params)
}

func (s *ProcessServer) Handle(ctx context.Context, conn *jsonrpc2.Conn, req *jsonrpc2.Request) {
	if req.Notif {
		var (
			err    error
			params any = req.Params
		)
		switch req.Method {
		case protocol.MethodTextDocumentPublishDiagnostics:
			params, err = s.handleTextDocumentPublishDiagnostics(ctx, req)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		s.proxyConn.Notify(ctx, req.Method, params)
		return
	}

	result, err := s.handle(ctx, req)
	if err != nil {
		if err := conn.ReplyWithError(ctx, req.ID, &jsonrpc2.Error{
			Code:    jsonrpc2.CodeInternalError,
			Message: err.Error(),
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		return
	}
	if err := conn.Reply(ctx, req.ID, result); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func (s *ProcessServer) handle(ctx context.Context, req *jsonrpc2.Request) (any, error) {
	var (
		err    error
		params any = req.Params
	)

	switch req.Method {
	case protocol.MethodInitialize:
		params, err = s.handleInitialize(ctx, req)
	case protocol.MethodClientRegisterCapability:
		params, err = s.handleClientRegisterCapability(ctx, req)
	}
	if err != nil {
		return nil, err
	}

	var result json.RawMessage

	if err := s.proxyConn.Call(ctx, req.Method, params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *ProcessServer) handleInitialize(_ context.Context, req *jsonrpc2.Request) (any, error) {
	var newParams protocol.InitializeParams

	if err := sonic.Unmarshal(*req.Params, &newParams); err != nil {
		return nil, err
	}
	if newParams.InitializationOptions != nil {
		options := make(map[string]any)
		b, err := sonic.Marshal(newParams.InitializationOptions)
		if err != nil {
			return nil, err
		}
		if err := sonic.Unmarshal(b, &options); err != nil {
			return nil, err
		}
		if opt, ok := options[s.name]; ok {
			newParams.InitializationOptions = opt
		}
		return newParams, nil
	}
	return req.Params, nil
}

func (s *ProcessServer) handleClientRegisterCapability(_ context.Context, req *jsonrpc2.Request) (any, error) {
	if s.name == "tailwindcss-language-server" {
		var newParams protocol.RegistrationParams

		if err := sonic.Unmarshal(*req.Params, &newParams); err != nil {
			return nil, err
		}
		for i := range newParams.Registrations {
			if newParams.Registrations[i].Method == protocol.MethodWorkspaceDidChangeWatchedFiles {
				newParams.Registrations[i].RegisterOptions = protocol.DidChangeWatchedFilesRegistrationOptions{
					Watchers: []protocol.FileSystemWatcher{
						{GlobPattern: "**/{tailwind,tailwind.config}.{js,cjs,ts,mjs}"},
						{GlobPattern: "**/{package-lock.json,yarn.lock,pnpm-lock.yaml}"},
						{GlobPattern: "**/*.{html,css,scss,sass,less,pcss}"},
					},
				}
			}
		}
		return newParams, nil
	}
	return req.Params, nil
}

func (s *ProcessServer) handleTextDocumentPublishDiagnostics(_ context.Context, req *jsonrpc2.Request) (any, error) {
	var params protocol.PublishDiagnosticsParams
	if err := sonic.Unmarshal(*req.Params, &params); err != nil {
		return nil, err
	}

	source := s.Name()
	for i := range params.Diagnostics {
		if params.Diagnostics[i].Source == "" {
			params.Diagnostics[i].Source = source
		}
	}

	s.diags.Set(params.URI, source, params.Diagnostics)

	return protocol.PublishDiagnosticsParams{
		URI:         params.URI,
		Diagnostics: s.diags.Get(params.URI),
	}, nil
}

func NewProcessServer(ctx context.Context, cmd *exec.Cmd, diags *DiagnosticCache) (*ProcessServer, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &ProcessServer{
		cmd:    cmd,
		name:   filepath.Base(cmd.Path),
		stdin:  stdout,
		stdout: stdin,
		diags:  diags,
	}

	proc.conn = jsonrpc2.NewConn(ctx, jsonrpc2.NewBufferedStream(proc, VSCodeObjectCodec{}), proc)
	return proc, nil
}
