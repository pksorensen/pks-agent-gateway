# Changelog

## [1.3.0](https://github.com/pksorensen/pks-agent-gateway/compare/pks-agent-gateway-v1.2.0...pks-agent-gateway-v1.3.0) (2026-10-16)


### Features

* **cli:** entra-cert credentials, principal app-id and limits, keyless subs ([c7f8e2c](https://github.com/pksorensen/pks-agent-gateway/commit/c7f8e2c6bfd49d4f0ef8cfb25ea1ef49397f0a6b))
* **gateway:** entra-cert credential source and identity init ([7c60f40](https://github.com/pksorensen/pks-agent-gateway/commit/7c60f4099cf80148c7705d7e996021239e8f83ba))
* **gateway:** entra-jwt caller auth on /apis/ ([77e93c4](https://github.com/pksorensen/pks-agent-gateway/commit/77e93c4ca9505989cdad8ad92a2848c6556d8db5))
* **gateway:** prod mode, kill switches, per-principal limits, body caps ([d7732ca](https://github.com/pksorensen/pks-agent-gateway/commit/d7732ca110875af42d9d8ee5385e655c3d07566f))


### Bug Fixes

* **gateway:** log entra-jwt rejection reasons; runbook container lookup ([8ea8ecc](https://github.com/pksorensen/pks-agent-gateway/commit/8ea8ecc5285ff5bc25e5dc613db01d6fee14d670))

## [1.2.0](https://github.com/pksorensen/pks-agent-gateway/compare/pks-agent-gateway-v1.1.0...pks-agent-gateway-v1.2.0) (2026-09-26)


### Features

* **gateway:** add realtime speech capability bridge ([#4](https://github.com/pksorensen/pks-agent-gateway/issues/4)) ([0dadc59](https://github.com/pksorensen/pks-agent-gateway/commit/0dadc596bde42ffcc7ed2ba620695435d82ad65b))
* **gateway:** APIM-style subscription lane, multi-tenant owners, runner connectors ([e82573d](https://github.com/pksorensen/pks-agent-gateway/commit/e82573dcbc8368a07d8df6b5ce3f6083e840e5e8))

## [1.1.0](https://github.com/pksorensen/pks-agent-gateway/compare/pks-agent-gateway-v1.0.0...pks-agent-gateway-v1.1.0) (2026-07-17)


### Features

* **gateway:** LLM simulator + testbench API for deterministic e2e ([1c3d40e](https://github.com/pksorensen/pks-agent-gateway/commit/1c3d40e4cf7bd782d4e6f60fe538c1bb1dda7ff8))

## 1.0.0 (2026-06-14)


### Features

* add gateway source (go.mod, main.go, Dockerfile) ([a7f2f81](https://github.com/pksorensen/pks-agent-gateway/commit/a7f2f81859bcace716873410dd7dfcedccac841d))
* **gateway:** add OTEL receiver, OIDC auth, and gateway-cli ([bb2b327](https://github.com/pksorensen/pks-agent-gateway/commit/bb2b32713ba28acdb155d0437d4e94df0dd3f48f))
* transparent streaming reverse proxy to api.anthropic.com ([3ea7b63](https://github.com/pksorensen/pks-agent-gateway/commit/3ea7b630cce95f00b314966a2b9cdcff5591531e))


### Bug Fixes

* stop .gitignore from excluding the src/gateway source dir ([c353015](https://github.com/pksorensen/pks-agent-gateway/commit/c3530151c6e669da9b8d199d6c0938c47ec6d29d))
