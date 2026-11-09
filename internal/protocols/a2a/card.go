package a2a

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// CardPath is where an agent publishes its Agent Card (both versions).
const CardPath = "/.well-known/agent-card.json"

// Skill is one thing an agent offers; Habakkuk lists each served agent as a
// skill.
type Skill struct {
	ID          string
	Name        string
	Description string
	Tags        []string
}

// CardInfo is what Habakkuk puts in, or reads from, an Agent Card.
type CardInfo struct {
	Name        string
	Description string
	Version     string // the agent's own version
	Endpoint    string // the JSON-RPC URL
	Protocol    Version
	Streaming   bool
	Bearer      bool // requests need a bearer token
	Skills      []Skill
	Schemes     []string // security scheme names, for display
}

// EncodeCard renders a card. A 0.3 card names its endpoint in url and
// preferredTransport; a 1.0 card in supportedInterfaces. The 0.3 card also
// carries supportedInterfaces, which 0.3 readers ignore, so a 1.0 client
// that did not send A2A-Version still finds the 1.0 interface.
func EncodeCard(v Version, c CardInfo) map[string]any {
	skills := make([]map[string]any, 0, len(c.Skills))
	for _, s := range c.Skills {
		tags := s.Tags
		if tags == nil {
			tags = []string{}
		}
		skills = append(skills, map[string]any{"id": s.ID, "name": s.Name, "description": s.Description, "tags": tags,
			"inputModes": []string{"text/plain"}, "outputModes": []string{"text/plain"}})
	}
	card := map[string]any{
		"name": c.Name, "description": c.Description, "version": c.Version,
		"provider":            map[string]any{"organization": "Habakkuk", "url": "https://github.com/habakkuk-ai/habakkuk"},
		"capabilities":        map[string]any{"streaming": c.Streaming, "pushNotifications": false},
		"defaultInputModes":   []string{"text/plain", "application/json"},
		"defaultOutputModes":  []string{"text/plain"},
		"skills":              skills,
		"supportedInterfaces": []map[string]any{{"url": c.Endpoint, "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}},
	}
	if v == V03 {
		card["protocolVersion"] = "0.3.0"
		card["url"] = c.Endpoint
		card["preferredTransport"] = "JSONRPC"
		card["capabilities"].(map[string]any)["stateTransitionHistory"] = false
	}
	if c.Bearer {
		if v == V03 {
			card["securitySchemes"] = map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}}
			card["security"] = []map[string]any{{"bearer": []string{}}}
		} else {
			card["securitySchemes"] = map[string]any{"bearer": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}
			card["securityRequirements"] = []map[string]any{{"schemes": map[string]any{"bearer": map[string]any{"list": []string{}}}}}
		}
	}
	return card
}

type wireInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
	Transport       string `json:"transport"` // 0.3 additionalInterfaces
}

type wireCard struct {
	Name                string          `json:"name"`
	Description         string          `json:"description"`
	Version             string          `json:"version"`
	ProtocolVersion     string          `json:"protocolVersion"`
	URL                 string          `json:"url"`
	PreferredTransport  string          `json:"preferredTransport"`
	AdditionalIfaces    []wireInterface `json:"additionalInterfaces"`
	SupportedInterfaces []wireInterface `json:"supportedInterfaces"`
	Capabilities        struct {
		Streaming bool `json:"streaming"`
	} `json:"capabilities"`
	SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
	Skills          []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"skills"`
}

// DecodeCard reads an Agent Card of either version and picks the JSON-RPC
// interface to use: 1.0 when the card offers it, else 0.3.
func DecodeCard(raw []byte) (*CardInfo, error) {
	var w wireCard
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("agent card: %w", err)
	}
	c := &CardInfo{Name: w.Name, Description: w.Description, Version: w.Version, Streaming: w.Capabilities.Streaming}
	for _, s := range w.Skills {
		c.Skills = append(c.Skills, Skill{ID: s.ID, Name: s.Name, Description: s.Description, Tags: s.Tags})
	}
	for name := range w.SecuritySchemes {
		c.Schemes = append(c.Schemes, name)
	}
	slices.Sort(c.Schemes)
	for _, i := range w.SupportedInterfaces {
		if strings.EqualFold(i.ProtocolBinding, "JSONRPC") && strings.HasPrefix(i.ProtocolVersion, "1") && i.URL != "" {
			c.Endpoint, c.Protocol = i.URL, V10
			return c, nil
		}
	}
	if w.URL != "" && (w.PreferredTransport == "" || strings.EqualFold(w.PreferredTransport, "JSONRPC")) {
		c.Endpoint, c.Protocol = w.URL, V03
		return c, nil
	}
	for _, i := range w.AdditionalIfaces {
		if strings.EqualFold(i.Transport, "JSONRPC") && i.URL != "" {
			c.Endpoint, c.Protocol = i.URL, V03
			return c, nil
		}
	}
	if w.Name == "" {
		return nil, fmt.Errorf("not an agent card")
	}
	return nil, fmt.Errorf("agent %q offers no JSON-RPC interface (Habakkuk speaks A2A over JSON-RPC only)", w.Name)
}
