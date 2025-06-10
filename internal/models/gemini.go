package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/genai"
)

// GeminiChatModel implements the eino ToolCallingChatModel interface for Google Gemini
type GeminiChatModel struct {
	client    *genai.Client
	model     string
	tools     []*genai.Tool
	origTools []*schema.ToolInfo
}

func NewGeminiChatModel(ctx context.Context, apiKey, modelName string) (*GeminiChatModel, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey: apiKey,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	return &GeminiChatModel{
		client: client,
		model:  modelName,
	}, nil
}

func (g *GeminiChatModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	chat, err := g.initChat(ctx, opts...)
	if err != nil {
		return nil, err
	}

	if len(input) == 0 {
		return nil, fmt.Errorf("input is empty")
	}

	parts, err := g.convertMessagesToParts(input)
	if err != nil {
		return nil, err
	}

	result, err := chat.SendMessage(ctx, parts...)
	if err != nil {
		return nil, fmt.Errorf("send message failed: %w", err)
	}

	return g.convertResponse(result)
}

func (g *GeminiChatModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	chat, err := g.initChat(ctx, opts...)
	if err != nil {
		return nil, err
	}

	if len(input) == 0 {
		return nil, fmt.Errorf("input is empty")
	}

	parts, err := g.convertMessagesToParts(input)
	if err != nil {
		return nil, err
	}

	resultIter := chat.SendMessageStream(ctx, parts...)

	sr, sw := schema.Pipe[*schema.Message](1)
	go func() {
		defer sw.Close()
		for resp, err := range resultIter {
			if err != nil {
				sw.Send(nil, err)
				return
			}
			message, err := g.convertResponse(resp)
			if err != nil {
				sw.Send(nil, err)
				return
			}
			if sw.Send(message, nil) {
				return
			}
		}
	}()

	return sr, nil
}

func (g *GeminiChatModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("no tools to bind")
	}

	gTools, err := g.convertTools(tools)
	if err != nil {
		return nil, fmt.Errorf("convert tools failed: %w", err)
	}

	newModel := *g
	newModel.tools = gTools
	newModel.origTools = tools
	return &newModel, nil
}

func (g *GeminiChatModel) BindTools(tools []*schema.ToolInfo) error {
	if len(tools) == 0 {
		return fmt.Errorf("no tools to bind")
	}

	gTools, err := g.convertTools(tools)
	if err != nil {
		return err
	}

	g.tools = gTools
	g.origTools = tools
	return nil
}

func (g *GeminiChatModel) BindForcedTools(tools []*schema.ToolInfo) error {
	return g.BindTools(tools)
}

func (g *GeminiChatModel) GetType() string {
	return "Gemini"
}

func (g *GeminiChatModel) IsCallbacksEnabled() bool {
	return false
}

func (g *GeminiChatModel) initChat(ctx context.Context, opts ...model.Option) (*genai.Chat, error) {
	// Process options to get tools
	commonOptions := model.GetCommonOptions(&model.Options{}, opts...)
	
	// Use tools from options if provided, otherwise use bound tools
	var tools []*genai.Tool
	if commonOptions.Tools != nil {
		var err error
		tools, err = g.convertTools(commonOptions.Tools)
		if err != nil {
			return nil, fmt.Errorf("convert tools from options failed: %w", err)
		}
	} else if len(g.tools) > 0 {
		tools = g.tools
	}
	
	// Create generation config with tools
	var config *genai.GenerateContentConfig
	if len(tools) > 0 {
		config = &genai.GenerateContentConfig{
			Tools: tools,
			ToolConfig: &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{
					Mode: genai.FunctionCallingConfigModeAuto,
				},
			},
		}
	}

	return g.client.Chats.Create(ctx, g.model, config, nil)
}

func (g *GeminiChatModel) convertTools(tools []*schema.ToolInfo) ([]*genai.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}

	// Create all function declarations
	funcDecls := make([]*genai.FunctionDeclaration, len(tools))
	for i, tool := range tools {
		funcDecl := &genai.FunctionDeclaration{
			Name:        tool.Name,
			Description: tool.Desc,
		}

		openSchema, err := tool.ToOpenAPIV3()
		if err != nil {
			return nil, fmt.Errorf("get open schema failed: %w", err)
		}

		funcDecl.Parameters, err = g.convertOpenAPISchema(openSchema)
		if err != nil {
			return nil, fmt.Errorf("convert open schema failed: %w", err)
		}

		funcDecls[i] = funcDecl
	}

	// Put all function declarations in a single tool
	return []*genai.Tool{
		{
			FunctionDeclarations: funcDecls,
		},
	}, nil
}

func (g *GeminiChatModel) convertOpenAPISchema(schema *openapi3.Schema) (*genai.Schema, error) {
	if schema == nil {
		return nil, nil
	}

	result := &genai.Schema{
		Format:      schema.Format,
		Description: schema.Description,
	}

	// Handle nullable
	if schema.Nullable {
		result.Nullable = &schema.Nullable
	}

	switch schema.Type {
	case openapi3.TypeObject:
		result.Type = genai.TypeObject
		if schema.Properties != nil {
			properties := make(map[string]*genai.Schema)
			for name, prop := range schema.Properties {
				if prop == nil || prop.Value == nil {
					continue
				}
				var err error
				properties[name], err = g.convertOpenAPISchema(prop.Value)
				if err != nil {
					return nil, err
				}
			}
			result.Properties = properties
		}
		if schema.Required != nil {
			result.Required = schema.Required
		}
	case openapi3.TypeArray:
		result.Type = genai.TypeArray
		if schema.Items != nil && schema.Items.Value != nil {
			var err error
			result.Items, err = g.convertOpenAPISchema(schema.Items.Value)
			if err != nil {
				return nil, err
			}
		}
	case openapi3.TypeString:
		result.Type = genai.TypeString
		if schema.Enum != nil {
			enums := make([]string, 0, len(schema.Enum))
			for _, e := range schema.Enum {
				if str, ok := e.(string); ok {
					enums = append(enums, str)
				}
			}
			result.Enum = enums
		}
	case openapi3.TypeNumber:
		result.Type = genai.TypeNumber
	case openapi3.TypeInteger:
		result.Type = genai.TypeInteger
	case openapi3.TypeBoolean:
		result.Type = genai.TypeBoolean
	default:
		result.Type = genai.TypeUnspecified
	}

	return result, nil
}

func (g *GeminiChatModel) convertMessagesToParts(messages []*schema.Message) ([]genai.Part, error) {
	var parts []genai.Part
	
	for _, message := range messages {
		if message.ToolCalls != nil {
			for _, call := range message.ToolCalls {
				args := make(map[string]any)
				err := json.Unmarshal([]byte(call.Function.Arguments), &args)
				if err != nil {
					return nil, fmt.Errorf("unmarshal tool call arguments failed: %w", err)
				}
				parts = append(parts, *genai.NewPartFromFunctionCall(call.Function.Name, args))
			}
		}

		if message.Role == schema.Tool {
			response := make(map[string]any)
			err := json.Unmarshal([]byte(message.Content), &response)
			if err != nil {
				return nil, fmt.Errorf("unmarshal tool response failed: %w", err)
			}
			parts = append(parts, *genai.NewPartFromFunctionResponse(message.ToolCallID, response))
		} else if message.Content != "" {
			parts = append(parts, *genai.NewPartFromText(message.Content))
		}
	}

	return parts, nil
}

func (g *GeminiChatModel) convertResponse(resp *genai.GenerateContentResponse) (*schema.Message, error) {
	if len(resp.Candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}

	candidate := resp.Candidates[0]
	message := &schema.Message{
		Role: schema.Assistant,
	}

	if candidate.Content != nil {
		var texts []string
		for _, part := range candidate.Content.Parts {
			// Check if it's a function call
			if part.FunctionCall != nil {
				args, err := json.Marshal(part.FunctionCall.Args)
				if err != nil {
					return nil, fmt.Errorf("marshal function call args failed: %w", err)
				}
				message.ToolCalls = append(message.ToolCalls, schema.ToolCall{
					ID: part.FunctionCall.Name,
					Function: schema.FunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(args),
					},
				})
			} else if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
		if len(texts) > 0 {
			message.Content = strings.Join(texts, "\n")
		}
	}

	return message, nil
}