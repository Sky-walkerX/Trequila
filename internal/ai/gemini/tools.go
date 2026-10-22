package gemini

import (
	"github.com/google/generative-ai-go/genai"
	"github.com/sky-walkerx/trequila/internal/ai"
)

func convertSchema(paramMap map[string]interface{}) *genai.Schema {
	if paramMap == nil {
		return nil
	}
	schema := &genai.Schema{}

	if t, ok := paramMap["type"].(string); ok {
		switch t {
		case "object":
			schema.Type = genai.TypeObject
		case "string":
			schema.Type = genai.TypeString
		case "integer":
			schema.Type = genai.TypeInteger
		case "number":
			schema.Type = genai.TypeNumber
		case "boolean":
			schema.Type = genai.TypeBoolean
		case "array":
			schema.Type = genai.TypeArray
		}
	}

	if desc, ok := paramMap["description"].(string); ok {
		schema.Description = desc
	}

	if props, ok := paramMap["properties"].(map[string]interface{}); ok {
		schema.Properties = make(map[string]*genai.Schema)
		for k, v := range props {
			if vMap, ok := v.(map[string]interface{}); ok {
				schema.Properties[k] = convertSchema(vMap)
			}
		}
	}

	if items, ok := paramMap["items"].(map[string]interface{}); ok {
		schema.Items = convertSchema(items)
	}

	if req, ok := paramMap["required"].([]interface{}); ok {
		for _, r := range req {
			if str, ok := r.(string); ok {
				schema.Required = append(schema.Required, str)
			}
		}
	}

	return schema
}

func convertTools(tools []ai.ToolDefinition) []*genai.Tool {
	if len(tools) == 0 {
		return nil
	}
	var decls []*genai.FunctionDeclaration
	for _, t := range tools {
		decl := &genai.FunctionDeclaration{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  convertSchema(t.Parameters),
		}
		decls = append(decls, decl)
	}
	return []*genai.Tool{{FunctionDeclarations: decls}}
}
