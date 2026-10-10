sed -i '' -e '/ReasoningTokens:  resp.Usage.ReasoningTokens,/a\
		CostKind:         resp.Usage.CostKind,\
' internal/llm/service/service.go
