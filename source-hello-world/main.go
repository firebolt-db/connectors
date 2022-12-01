package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/estuary/flow/go/protocols/airbyte"
)

type Config struct {
	Greetings int `json:"greetings"`
}

type State struct {
	Cursor int `json:"cursor"`
}

func (c *Config) Validate() error {
	return nil
}

func (c *State) Validate() error {
	return nil
}

const configSchema = `{
	"$schema": "http://json-schema.org/draft-07/schema#",
	"title":   "An Infinite Stream of Hello Worlds",
	"type":    "object",
	"properties": {
		"greetings": {
			"type": ["integer", "null"],
			"default": null
		}
	}
}`

const greetingSchema = `{
	"type":"object",
	"properties": {
		"count":   {"type": "integer"},
		"message": {"type": "string"}
	},
	"required": ["count", "message"]
}`

func main() {
	airbyte.RunMain(spec, doCheck, doDiscover, doRead)
}

var spec = airbyte.Spec{
	SupportsIncremental:           true,
	SupportedDestinationSyncModes: airbyte.AllDestinationSyncModes,
	ConnectionSpecification:       json.RawMessage(configSchema),
	DocumentationURL:              "https://go.estuary.dev/source-hello-world",
}

func doCheck(args airbyte.CheckCmd) error {
	var result = &airbyte.ConnectionStatus{
		Status: airbyte.StatusSucceeded,
	}

	if err := args.ConfigFile.Parse(new(Config)); err != nil {
		result.Status = airbyte.StatusFailed
		result.Message = err.Error()
	}

	return airbyte.NewStdoutEncoder().Encode(airbyte.Message{
		Type:             airbyte.MessageTypeConnectionStatus,
		ConnectionStatus: result,
	})
}

func doDiscover(args airbyte.DiscoverCmd) error {
	if err := args.ConfigFile.Parse(new(Config)); err != nil {
		return err
	}

	var catalog = new(airbyte.Catalog)
	catalog.Streams = append(catalog.Streams, airbyte.Stream{
		Name:                    "greetings",
		JSONSchema:              json.RawMessage(greetingSchema),
		SupportedSyncModes:      []airbyte.SyncMode{airbyte.SyncModeIncremental},
		SourceDefinedCursor:     true,
		SourceDefinedPrimaryKey: [][]string{{"count"}},
	})

	var encoder = airbyte.NewStdoutEncoder()
	return encoder.Encode(airbyte.Message{
		Type:    airbyte.MessageTypeCatalog,
		Catalog: catalog,
	})
}

func doRead(args airbyte.ReadCmd) error {
	var config Config
	var state State
	var catalog airbyte.ConfiguredCatalog

	if err := args.ConfigFile.Parse(&config); err != nil {
		return err
	} else if err := args.CatalogFile.Parse(&catalog); err != nil {
		return err
	} else if args.StateFile != "" {
		if err := args.StateFile.Parse(&state); err != nil {
			return err
		}
	}

	var enc = airbyte.NewStdoutEncoder()
	var now = time.Now()
	for {
		var b, err = json.Marshal(struct {
			Count   int    `json:"count"`
			Message string `json:"message"`
		}{
			state.Cursor,
			fmt.Sprintf("Hello #%d", state.Cursor),
		})
		if err != nil {
			return err
		}
		state.Cursor++

		if err = enc.Encode(&airbyte.Message{
			Type: airbyte.MessageTypeRecord,
			Record: &airbyte.Record{
				Stream:    "greetings",
				EmittedAt: now.UTC().UnixNano() / int64(time.Millisecond),
				Data:      b,
			},
		}); err != nil {
			return err
		}

		if b, err = json.Marshal(state); err != nil {
			return err
		} else if err = enc.Encode(airbyte.Message{
			Type:  airbyte.MessageTypeState,
			State: &airbyte.State{Data: b},
		}); err != nil {
			return err
		}

		now = <-time.After(time.Millisecond * 500)
	}
}
