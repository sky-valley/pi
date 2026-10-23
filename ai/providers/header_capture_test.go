package providers

// completionsDoneSSE is a minimal Chat Completions stream that ends at once.
const completionsDoneSSE = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
