package ai

import "context"

// Cloudflare provider auth, ported from pi providers/cloudflare-auth.ts
// (cloudflareWorkersAIAuth, cloudflareAIGatewayAuth; upstream 129eb460c,
// 49fbe6834, bdd5c53bc, 9993c9690, fed6009cc). Both providers need the
// account id as well as the key, and AI Gateway also needs the gateway id.
// The ids are endpoint configuration, not auth: they travel in the result's
// Env, which fills the {CLOUDFLARE_ACCOUNT_ID}/{CLOUDFLARE_GATEWAY_ID}
// placeholders of the models' base URLs.

const (
	cloudflareAPIKeyEnv    = "CLOUDFLARE_API_KEY"
	cloudflareAccountIDEnv = "CLOUDFLARE_ACCOUNT_ID"
	cloudflareGatewayIDEnv = "CLOUDFLARE_GATEWAY_ID"
)

// cloudflareValue resolves one Cloudflare setting per field: the credential's
// value when it has one (its key, or an entry of its env), else the ambient
// environment. A credential that carries only the key still picks up the
// account and gateway ids from the environment (upstream bdd5c53bc). A value
// the credential holds is used as is, even when empty.
func cloudflareValue(ctx context.Context, name string, authCtx AuthContext, credential *Credential) (string, error) {
	if credential != nil {
		if name == cloudflareAPIKeyEnv {
			return credential.Key, nil
		}
		if value, ok := credential.Env[name]; ok {
			return value, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	value := authCtx.Env(name)
	return value, ctx.Err()
}

// resolveCloudflareAuth resolves the key, the account id and, when gateway is
// set, the gateway id (pi resolveCloudflareEnv). It reports nil — the provider
// is not configured — unless every one of them is non-empty.
func resolveCloudflareAuth(ctx context.Context, gateway bool, authCtx AuthContext, credential *Credential) (apiKey string, env map[string]string, source string, err error) {
	if apiKey, err = cloudflareValue(ctx, cloudflareAPIKeyEnv, authCtx, credential); err != nil {
		return "", nil, "", err
	}
	accountID, err := cloudflareValue(ctx, cloudflareAccountIDEnv, authCtx, credential)
	if err != nil {
		return "", nil, "", err
	}
	var gatewayID string
	if gateway {
		if gatewayID, err = cloudflareValue(ctx, cloudflareGatewayIDEnv, authCtx, credential); err != nil {
			return "", nil, "", err
		}
	}
	if apiKey == "" || accountID == "" || (gateway && gatewayID == "") {
		return "", nil, "", nil
	}
	env = map[string]string{cloudflareAccountIDEnv: accountID}
	if gatewayID != "" {
		env[cloudflareGatewayIDEnv] = gatewayID
	}
	source = cloudflareAPIKeyEnv
	if credential != nil {
		source = "stored credential"
	}
	return apiKey, env, source, nil
}

// cloudflareLogin prompts for the key and then for each id in envNames, and
// returns them as one api-key credential (pi's Cloudflare login).
func cloudflareLogin(ctx context.Context, interaction AuthInteraction, prompts []AuthPrompt, envNames []string) (*Credential, error) {
	answers := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		answer, err := interaction.Prompt(prompt)
		if err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env := make(map[string]string, len(envNames))
	for i, name := range envNames {
		env[name] = answers[i+1]
	}
	return &Credential{Type: CredentialAPIKey, Key: answers[0], Env: env}, nil
}

var (
	cloudflareKeyPrompt       = AuthPrompt{Type: AuthPromptSecret, Message: "Enter Cloudflare API key"}
	cloudflareAccountIDPrompt = AuthPrompt{Type: AuthPromptText, Message: "Enter Cloudflare account ID"}
	cloudflareGatewayIDPrompt = AuthPrompt{Type: AuthPromptText, Message: "Enter Cloudflare AI Gateway ID"}
)

// cloudflareWorkersAIAuth is cloudflare-workers-ai's api-key auth (pi
// cloudflareWorkersAIAuth): the key authenticates the request, and the
// account id goes to Env.
func cloudflareWorkersAIAuth() *ApiKeyAuth {
	return &ApiKeyAuth{
		Name: "Cloudflare API key",
		Login: func(ctx context.Context, interaction AuthInteraction) (*Credential, error) {
			return cloudflareLogin(ctx, interaction,
				[]AuthPrompt{cloudflareKeyPrompt, cloudflareAccountIDPrompt},
				[]string{cloudflareAccountIDEnv})
		},
		Resolve: func(ctx context.Context, authCtx AuthContext, credential *Credential) (*AuthResult, error) {
			apiKey, env, source, err := resolveCloudflareAuth(ctx, false, authCtx, credential)
			if err != nil || env == nil {
				return nil, err
			}
			return &AuthResult{Auth: ModelAuth{APIKey: apiKey}, Env: env, Source: source}, nil
		},
	}
}

// cloudflareAIGatewayAuth is cloudflare-ai-gateway's api-key auth (pi
// cloudflareAIGatewayAuth). The gateway authenticates the caller by
// cf-aig-authorization, so the key is header-owned auth with no api key at
// all, and deletion markers keep the upstream provider's own auth headers off
// the request (upstream a24fb9e96, #7030). Both ids go to Env.
func cloudflareAIGatewayAuth() *ApiKeyAuth {
	return &ApiKeyAuth{
		Name: "Cloudflare API key",
		Login: func(ctx context.Context, interaction AuthInteraction) (*Credential, error) {
			return cloudflareLogin(ctx, interaction,
				[]AuthPrompt{cloudflareKeyPrompt, cloudflareAccountIDPrompt, cloudflareGatewayIDPrompt},
				[]string{cloudflareAccountIDEnv, cloudflareGatewayIDEnv})
		},
		Resolve: func(ctx context.Context, authCtx AuthContext, credential *Credential) (*AuthResult, error) {
			apiKey, env, source, err := resolveCloudflareAuth(ctx, true, authCtx, credential)
			if err != nil || env == nil {
				return nil, err
			}
			gatewayAuth := "Bearer " + apiKey
			return &AuthResult{
				Auth: ModelAuth{Headers: ProviderHeaders{
					"cf-aig-authorization": &gatewayAuth,
					"Authorization":        nil,
					"x-api-key":            nil,
				}},
				Env:    env,
				Source: source,
			}, nil
		},
	}
}
