// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package selfmaint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/moby/moby/client"

	"github.com/ianglennon/bayplan/proto/wire"
)

// maxServiceJSON bounds what is read of a service inspection.
const maxServiceJSON = 8 << 20

// updateSwarm rolls the agent's own service to image. The service ID is the one
// swarm put on the agent's own container (com.docker.swarm.service.id), never one the
// server named. The spec is patched as raw JSON, the image field only, and posted with
// the version it was inspected at, so swarm performs its rolling update (with the
// parallelism and delay the install set) and no field this client has no type for is
// dropped (RESEARCH Pitfall 19, A14). It returns an update_result error code, or "".
func (m *Maint) updateSwarm(ctx context.Context, cli *client.Client, serviceID, image string) string {
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		m.log.Error("self-update: cannot read the engine's swarm state", "error", err)
		return wire.UpdateErrFailed
	}
	if !info.Info.Swarm.ControlAvailable {
		m.log.Warn("self-update: this agent runs on a worker, which cannot update the service")
		return wire.UpdateErrNotAManager
	}

	hc := unixHTTPClient(m.cfg.SocketPath)
	defer hc.CloseIdleConnections()

	// Both calls use the engine's unversioned paths, so the answer of the one and
	// the body of the other are the same API version.
	inspected, err := httpDo(ctx, hc, http.MethodGet, "/services/"+url.PathEscape(serviceID), nil)
	if err != nil {
		m.log.Error("self-update: cannot inspect the agent's own service", "service", serviceID, "error", err)
		return wire.UpdateErrFailed
	}
	spec, version, err := patchServiceImage(inspected, image)
	if err != nil {
		m.log.Error("self-update: cannot patch the service spec", "service", serviceID, "error", err)
		return wire.UpdateErrFailed
	}
	path := "/services/" + url.PathEscape(serviceID) + "/update?version=" + url.QueryEscape(version)
	if _, err := httpDo(ctx, hc, http.MethodPost, path, spec); err != nil {
		m.log.Error("self-update: the service update failed", "service", serviceID, "error", err)
		return wire.UpdateErrFailed
	}
	m.log.Info("self-update: the service was updated; swarm is rolling the agents", "service", serviceID, "image", image)
	return ""
}

// httpDo sends one request to the engine and returns the body of a 2xx answer.
func httpDo(ctx context.Context, hc *http.Client, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxServiceJSON))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(b)
		if len(msg) > 256 {
			msg = msg[:256]
		}
		return nil, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	return b, nil
}

// patchServiceImage takes the JSON of GET /services/{id} and returns the body for
// POST /services/{id}/update (the Spec with Spec.TaskTemplate.ContainerSpec.Image
// set to image) and the version index to post it with. Every other value of the
// spec is carried as the engine's own bytes (json.RawMessage), so a field this
// client does not know, and a number a float would round, come through unchanged.
func patchServiceImage(inspected []byte, image string) (spec []byte, version string, err error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(inspected, &top); err != nil {
		return nil, "", fmt.Errorf("read the service: %w", err)
	}
	var v struct {
		Index json.Number `json:"Index"`
	}
	if err := json.Unmarshal(top["Version"], &v); err != nil || v.Index == "" {
		return nil, "", fmt.Errorf("the service has no version index")
	}
	if _, err := v.Index.Int64(); err != nil {
		return nil, "", fmt.Errorf("the service version index is not a number")
	}
	raw, ok := top["Spec"]
	if !ok {
		return nil, "", fmt.Errorf("the service has no spec")
	}
	val, err := json.Marshal(image)
	if err != nil {
		return nil, "", err
	}
	spec, err = setPath(raw, []string{"TaskTemplate", "ContainerSpec", "Image"}, val)
	if err != nil {
		return nil, "", err
	}
	return spec, v.Index.String(), nil
}

// setPath replaces the value at path in a JSON object, which must already exist.
func setPath(obj json.RawMessage, path []string, value json.RawMessage) (json.RawMessage, error) {
	if len(path) == 0 {
		return nil, errNoImage
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return nil, errNoImage
	}
	cur, ok := m[path[0]]
	if !ok {
		return nil, errNoImage
	}
	if len(path) == 1 {
		m[path[0]] = value
	} else {
		next, err := setPath(cur, path[1:], value)
		if err != nil {
			return nil, err
		}
		m[path[0]] = next
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
