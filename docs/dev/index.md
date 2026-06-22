# Developer Docs

This chapter is for engineers **working on the CubeSandbox codebase** itself — contributors, maintainers, and integrators of the internal services (CubeMaster, CubeProxy, CubeAPI, Cubelet). It collects the conventions, contracts, and internal references you need to change the system safely.

If you are *using* CubeSandbox (deploying it, building templates, calling the API), the [Guide](../guide/introduction) is the better starting point. The [Architecture](../architecture/overview) section explains the system design; this chapter goes one level deeper into the rules that govern how the code is written and how the services cooperate.

## Conventions

- [Redis Key Convention](./redis-key-spec) — the unified namespace every service must use for the shared Redis instance: naming format, scope ownership, the registered key catalog, TTL policy, and the per-service key-builder modules.

## What belongs here

- Cross-service data contracts and naming conventions (keys, topics, schemas)
- Internal module boundaries and where to add new behavior (e.g. key builders, cache layers)
- Coding standards and contribution rules specific to the internal services
- Reserved for future pages: internal APIs, testing conventions, contribution guide

::: tip Bilingual parity
Developer docs are maintained in both English (`docs/dev/`) and Chinese (`docs/zh/dev/`). When you add or update a page, keep both languages in sync and use the same filename so the URLs stay aligned.
:::
