package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	schemagen "github.com/estuary/connectors/go/schema-gen"
	boilerplate "github.com/estuary/connectors/materialize-boilerplate"
	"github.com/estuary/flow/go/protocols/fdb/tuple"
	pf "github.com/estuary/flow/go/protocols/flow"
	pm "github.com/estuary/flow/go/protocols/materialize"
	log "github.com/sirupsen/logrus"
	"go.gazette.dev/core/consumer/protocol"
)

// driver implements the pm.DriverServer interface.
type driver struct{}

type config struct {
	SenderConfig SlackSenderConfig `json:"sender_config" jsonschema:"title=Slack Config"`
	Credentials  CredentialConfig  `json:"credentials" jsonschema:"title=Authentication" jsonschema_extras:"x-oauth2-provider=slack"`
}

// Validate returns an error if the config is not well-formed.
func (c config) Validate() error {
	if err := c.Credentials.validateClientCreds(); err != nil {
		return err
	}
	return nil
}

func (c config) buildAPI() (*SlackAPI, error) {
	var api = c.Credentials.SlackAPI(c.SenderConfig)
	if err := api.AuthTest(); err != nil {
		return nil, fmt.Errorf("error verifying authentication: %w", err)
	}
	return api, nil
}

type resource struct {
	Channel string `json:"channel" jsonschema:"title=Channel,description=The name of the channel to post messages to (or a raw channel ID like \"id:C123456\")"`
}

func (r resource) Validate() error {
	if r.Channel == "" {
		return fmt.Errorf("missing required channel name/id")
	}
	return nil
}

type driverCheckpoint struct {
	LastMessage time.Time `json:"last_message"`
}

func (c driverCheckpoint) Validate() error {
	return nil
}

func (driver) Spec(ctx context.Context, req *pm.Request_Spec) (*pm.Response_Spec, error) {
	log.Debug("handling Spec request")

	endpointSchema, err := schemagen.GenerateSchema("Slack Connection", &config{}).MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating endpoint schema: %w", err)
	}

	resourceSchema, err := schemagen.GenerateSchema("Slack Channel", &resource{}).MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("generating resource schema: %w", err)
	}

	return &pm.Response_Spec{
		ConfigSchemaJson:         json.RawMessage(endpointSchema),
		ResourceConfigSchemaJson: json.RawMessage(resourceSchema),
		DocumentationUrl:         "https://go.estuary.dev/materialize-slack",
		Oauth2:                   Spec("channels:read", "groups:read", "im:read", "channels:join", "chat:write", "chat:write.customize"),
		//	                      Spec("channels:history", "channels:join", "channels:read", "files:read", "groups:read", "links:read", "reactions:read", "remote_files:read", "team:read", "usergroups:read", "users.profile:read", "users:read"),
	}, nil
}

func (driver) Validate(ctx context.Context, req *pm.Request_Validate) (*pm.Response_Validated, error) {
	log.Debug("handling Validate request")

	var cfg config
	if err := pf.UnmarshalStrict(req.ConfigJson, &cfg); err != nil {
		return nil, err
	}

	var api, err = cfg.buildAPI()
	if err != nil {
		return nil, err
	}

	var out []*pm.Response_Validated_Binding
	for _, binding := range req.Bindings {
		var res resource
		if err := pf.UnmarshalStrict(binding.ResourceConfigJson, &res); err != nil {
			return nil, fmt.Errorf("parsing resource config: %w", err)
		}
		var channelInfo, err = api.ConversationInfo(res.Channel)
		if err != nil {
			return nil, fmt.Errorf("error getting channel: %s, %w", res.Channel, err)
		}

		if !channelInfo.Channel.IsMember {
			if err := api.JoinChannel(res.Channel); err != nil {
				return nil, fmt.Errorf("error joining channel: %s, %w", res.Channel, err)
			}
		}

		var constraints = make(map[string]*pm.Response_Validated_Constraint)
		for _, projection := range binding.Collection.Projections {
			constraints[projection.Field] = &pm.Response_Validated_Constraint{
				Type:   pm.Response_Validated_Constraint_FIELD_OPTIONAL,
				Reason: "All fields other than 'ts' and 'message' will be ignored",
			}
		}
		constraints["ts"] = &pm.Response_Validated_Constraint{
			Type:   pm.Response_Validated_Constraint_LOCATION_REQUIRED,
			Reason: "The Slack materialization requires a message timestamp",
		}
		constraints["text"] = &pm.Response_Validated_Constraint{
			Type:   pm.Response_Validated_Constraint_LOCATION_RECOMMENDED,
			Reason: "The Slack materialization requires either text or blocks",
		}
		constraints["blocks"] = &pm.Response_Validated_Constraint{
			Type:   pm.Response_Validated_Constraint_LOCATION_RECOMMENDED,
			Reason: "The Slack materialization requires either text or blocks",
		}
		out = append(out, &pm.Response_Validated_Binding{
			Constraints:  constraints,
			DeltaUpdates: true,
			ResourcePath: []string{res.Channel},
		})
	}

	return &pm.Response_Validated{Bindings: out}, nil
}

func (driver) Apply(ctx context.Context, req *pm.Request_Apply) (*pm.Response_Applied, error) {
	log.Debug("handling Apply request")
	return &pm.Response_Applied{ActionDescription: "materialize-slack does not modify channels"}, nil
}

// Transactions implements the DriverServer interface.
func (driver) NewTransactor(ctx context.Context, open pm.Request_Open) (pm.Transactor, *pm.Response_Opened, error) {
	log.Debug("handling Transactions request")

	var cfg config
	if err := pf.UnmarshalStrict(open.Materialization.ConfigJson, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parsing endpoint config: %w", err)
	}

	var checkpoint driverCheckpoint
	if err := pf.UnmarshalStrict(open.StateJson, &checkpoint); err != nil {
		return nil, nil, fmt.Errorf("parsing driver checkpoint: %w", err)
	}
	if checkpoint.LastMessage.IsZero() {
		checkpoint.LastMessage = time.Now()
	}

	api, err := cfg.buildAPI()
	if err != nil {
		return nil, nil, err
	}

	var bindings []*binding
	for _, b := range open.Materialization.Bindings {
		var fields = append(append([]string{}, b.FieldSelection.Keys...), b.FieldSelection.Values...)
		var fieldIndices = make(map[string]int)
		for idx, field := range fields {
			fieldIndices[field] = idx
		}

		tsIndex, ok := fieldIndices["ts"]
		if !ok {
			return nil, nil, fmt.Errorf("no index found for field 'ts' in %q binding", b.ResourcePath[0])
		}
		var textIndex, textOk = fieldIndices["text"]
		var blocksIndex, blocksOk = fieldIndices["blocks"]
		if !(textOk || blocksOk) {
			return nil, nil, fmt.Errorf("no index found for fields 'text' or 'blocks' in %q binding", b.ResourcePath[0])
		}
		bindings = append(bindings, &binding{
			channel:     b.ResourcePath[0],
			tsIndex:     tsIndex,
			textIndex:   textIndex,
			blocksIndex: blocksIndex,
		})
	}

	var transactor = &transactor{
		api:         api,
		lastMessage: checkpoint.LastMessage,
		bindings:    bindings,
	}

	return transactor, &pm.Response_Opened{}, nil
}

type transactor struct {
	api         *SlackAPI
	bindings    []*binding
	lastMessage time.Time
}

type binding struct {
	channel     string
	tsIndex     int
	textIndex   int
	blocksIndex int
}

func (d *transactor) Load(it *pm.LoadIterator, loaded func(int, json.RawMessage) error) error {
	log.Debug("handling Load operation")
	return fmt.Errorf("this materialization only supports delta-updates")
}

func (t *transactor) Store(it *pm.StoreIterator) (pm.StartCommitFunc, error) {
	log.Debug("handling Store operation")

	var vals []tuple.TupleElement
	for it.Next() {
		// Concatenate key and non-key fields because we don't care
		vals = append(append(vals[:0], it.Key...), it.Values...)

		var b = t.bindings[it.Binding]

		log.WithField("binding", fmt.Sprintf("%#v", b)).WithField("vals", fmt.Sprintf("%#v", vals)).Debug("storing document")

		// Extract timestamp and message fields from the document
		tsStr, ok := vals[b.tsIndex].(string)
		if !ok {
			return nil, fmt.Errorf("timestamp field is not a string, instead got %#v", vals[b.tsIndex])
		}
		var text, textExists = vals[b.textIndex].(string)
		var blocks, blocksExists = vals[b.blocksIndex].(string)
		if !(textExists || blocksExists) {
			return nil, fmt.Errorf("text or blocks fields missing, instead got text:%#v, blocks:%#v", vals[b.textIndex], vals[b.blocksIndex])
		}

		// Parse the timstamp as a time.Time
		ts, err := time.Parse(time.RFC3339Nano, tsStr)
		if err != nil {
			return nil, fmt.Errorf("invalid timestamp %q", tsStr)
		}

		var blocksParsed json.RawMessage
		if blocksExists {
			err = blocksParsed.UnmarshalJSON([]byte(blocks))

			if err != nil {
				return nil, fmt.Errorf("invalid blocks value %q", tsStr)
			}
		}

		if ts.After(t.lastMessage) {
			if err := t.api.PostMessage(b.channel, text, blocksParsed); err != nil {
				return nil, fmt.Errorf("error sending message: %w", err)
			}
			t.lastMessage = ts
		}
	}

	return func(ctx context.Context, runtimeCheckpoint *protocol.Checkpoint, runtimeAckCh <-chan struct{}) (*pf.ConnectorState, pf.OpFuture) {
		log.Debug("handling Prepare operation")
		var checkpoint = driverCheckpoint{LastMessage: t.lastMessage}
		var bs, err = json.Marshal(&checkpoint)
		if err != nil {
			return nil, pf.FinishedOperation(fmt.Errorf("error marshalling driver checkpoint: %w", err))
		}

		return &pf.ConnectorState{
			UpdatedJson: json.RawMessage(bs),
			MergePatch:  true,
		}, nil
	}, nil
}

func (transactor) Commit(context.Context) error {
	log.Debug("handling Commit operation")
	return nil
}

func (transactor) Acknowledge(context.Context) error {
	log.Debug("handling Acknowledge operation")
	return nil
}

func (transactor) Destroy() {
	log.Debug("handling Destroy operation")
}

func main() {
	log.SetLevel(log.DebugLevel)
	log.Info("connector starting")
	boilerplate.RunMain(new(driver))
}
