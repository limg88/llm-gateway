package main

import "encoding/json"

type wireUsage struct {
	In              *int64 `json:"input_tokens"`
	Out             *int64 `json:"output_tokens"`
	Prompt          *int64 `json:"prompt_tokens"`
	Completion      *int64 `json:"completion_tokens"`
	CacheRead       int64  `json:"cache_read_input_tokens"`
	CacheWrite      int64  `json:"cache_creation_input_tokens"`
	ResponseDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	Creation struct {
		Hour int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	Details struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}
type meter struct {
	units                       Units
	input, output, done, failed bool
}

func (m *meter) observe(data []byte, endpoint string) {
	var v struct {
		Type    string     `json:"type"`
		Usage   *wireUsage `json:"usage"`
		Message struct {
			Usage *wireUsage `json:"usage"`
		} `json:"message"`
		Response struct {
			Usage *wireUsage `json:"usage"`
		} `json:"response"`
	}
	if string(data) == "[DONE]" {
		m.done = true
		return
	}
	if json.Unmarshal(data, &v) != nil {
		return
	}
	if v.Type == "error" || v.Type == "response.failed" {
		m.failed = true
	}
	if v.Type == "message_stop" || v.Type == "response.completed" {
		m.done = true
	}
	u := v.Usage
	if u == nil {
		u = v.Message.Usage
	}
	if u == nil {
		u = v.Response.Usage
	}
	if u == nil {
		return
	}
	if u.Prompt != nil {
		m.units.Input = *u.Prompt - u.Details.Cached
		m.units.Read = u.Details.Cached
		m.input = true
	}
	if u.Completion != nil {
		m.units.Output = *u.Completion
		m.output = true
	}
	if u.In != nil {
		m.units.Input = *u.In
		m.units.Read = u.CacheRead
		if endpoint == "responses" {
			m.units.Read = u.ResponseDetails.Cached
			m.units.Input -= m.units.Read
		}
		m.units.Write = u.CacheWrite - u.Creation.Hour
		m.units.Hour = u.Creation.Hour
		m.input = true
	}
	if u.Out != nil {
		m.units.Output = *u.Out
		m.output = true
	}
	if endpoint == "embeddings" {
		m.output = true
	}
}
func (m *meter) result() *Units {
	if !m.input || !m.output {
		return nil
	}
	u := m.units
	return &u
}
