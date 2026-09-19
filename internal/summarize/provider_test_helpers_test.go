package summarize

func providerTestRequest() Request {
	return Request{System: "system", Goal: "goal", Messages: []byte(`[{"text":"prompt"}]`)}
}
