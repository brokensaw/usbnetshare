package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrUnauthorized: el token no coincide con el del registro.
var ErrUnauthorized = errors.New("el registro rechazó el token (¿se regeneró? copia el que muestra el contenedor)")

// Client habla con usbnetshare-registry.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

func NewClient(url, token string) *Client {
	return &Client{base: strings.TrimRight(url, "/"), token: token, hc: &http.Client{Timeout: 10 * time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("no se puede contactar con el registro %s: %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("registro: %s", e.Error)
		}
		return fmt.Errorf("registro: %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// Heartbeat envía el latido y devuelve el intervalo pedido por el registro.
func (c *Client) Heartbeat(ctx context.Context, hb Heartbeat) (time.Duration, error) {
	var r HeartbeatReply
	if err := c.do(ctx, "POST", "/api/v1/heartbeat", hb, &r); err != nil {
		return 0, err
	}
	return time.Duration(r.IntervalSeconds) * time.Second, nil
}

// Peers devuelve los equipos registrados.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var p []Peer
	err := c.do(ctx, "GET", "/api/v1/peers", nil, &p)
	return p, err
}
