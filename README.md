# cpa-muse

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) plugin that serves Meta Muse models (`muse-spark-*`, `muse-wonder-*`) from your Muse Code subscription. It works with an unmodified CPA (tested with v8.0.54) and accepts OpenAI Chat Completions, OpenAI Responses, and Claude Messages requests.

## Install

CPA needs a glibc Linux build (including the official Docker image) or macOS; its musl `no-plugin` builds can't load plugins. Release builds exist for linux and darwin, amd64 and arm64.

Add this plugin's registry and the usual settings to CPA's `config.yaml`:

```yaml
management:
  secret-key: "choose-a-management-key"   # required for install and login

routing:
  session-affinity: true                  # recommended

plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/sbga-tech/cpa-muse/main/registry.json"
```

Then install Muse from the plugin store in CPA's management panel, or:

```sh
curl -s -X POST http://127.0.0.1:8317/v8/management/plugins/store/muse/install \
  -H "Authorization: Bearer $MANAGEMENT_KEY" -H 'Content-Type: application/json' -d '{}'
# {"status":"installed","version":"0.1.0","restart_required":false,...}
```

CPA downloads the latest [release](https://github.com/sbga-tech/cpa-muse/releases), checks its checksum, enables the plugin, and loads it without a restart.

- **Update**: run the same install again (or click update in the panel) after a new release; the new version loads without a restart.
- **Pin or roll back**: send `{"version":"0.1.0"}` as the body.
- **Manual install**: unzip a release asset into `plugins/<os>/<arch>/` and enable `plugins.configs.muse.enabled: true`, or build it yourself with `make build`.

## Log in

Start a login through CPA's management API:

```sh
curl -s -X POST http://127.0.0.1:8317/v8/management/oauth/auth-url \
  -H "Authorization: Bearer $MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  -d '{"provider":"muse"}'
# {"status":"ok","url":"https://auth.meta.com/oauth/device/?code=ABCD-EFGH","state":"muse-…"}
```

Open the `url` and approve the sign-in. Then check the result:

```sh
curl -s "http://127.0.0.1:8317/v8/management/oauth/status?state=muse-…" \
  -H "Authorization: Bearer $MANAGEMENT_KEY"
# {"status":"wait"} until you approve, then {"status":"ok"}
```

CPA saves the account as `muse-<email>-<id>.json` in its `oauth.auth-dir` and uses it immediately. Repeat for more accounts; CPA balances and fails over between them.

- **Renewing an account**: if an account stops working and needs to sign in again, repeat the login with `{"provider":"muse","name":"<file name>"}`. The file is updated in place and keeps your settings. Signing in with a different Meta account is refused.
- **"Would not issue a Model API key"**: your Meta account isn't set up for API access yet. Finish onboarding at https://dev.meta.ai, then log in again.
- **Don't pass `proxy_url`** when starting the login; see [Limitations](#limitations).

## Migrating from CPA's built-in Meta provider

Accounts created with `-meta-login` keep using CPA's built-in Meta provider until you migrate them. For each account file:

1. Stop CPA.
2. Change `"type": "meta"` to `"type": "muse"` in the file.
3. Start CPA.

CPA must be stopped (or restarted right after the edit); otherwise the account disappears until the next restart. If you previously listed accounts in the plugin config (`api-keys`, `host-credentials`, `meta-limits`), move each one into its own auth file and remove those keys; the plugin reports a configuration error and won't load while they are present. A minimal hand-written file:

```json
{ "type": "muse", "api_key": "YOUR_META_API_KEY", "dca_token": "dca:…", "priority": 10 }
```

Migrated accounts start a fresh per-account history in usage dashboards such as CPA Usage Keeper; totals and costs are unaffected.

## Configuration

All keys are optional and go under `plugins.configs.muse`. Account selection, priority, and session affinity use CPA's normal settings.

| Key | Default | Meaning |
|---|---|---|
| `models` | – | Extra model IDs to accept beyond the built-in list. |
| `prefixes` | – | Model prefixes to accept, e.g. `meta` for `meta/muse-spark-1.3`. |
| `aliases` | – | Map your own model names to Muse models. |
| `client` | Muse Code 1.4.14 | Override the reported client: `version`, `build`, `surface`, `os`, `arch`. |
| `muse-namespace` | `true` | Add Muse's tool namespace to requests that use tools. |
| `meta-responses.mode` | `legacy` | How Responses requests are sent: `legacy`, `native`, or `disabled`. |
| `meta-responses.header-profile` | `generic` | Headers in native mode: `generic` or `muse`. |
| `meta-messages.mode` | `legacy` | How Claude Messages requests are sent: `legacy`, `enabled` (native), or `disabled`. |
| `meta-messages.*` | – | Native Messages options: `allow-subscription`, `count-tokens` (`local` or `upstream-then-local`), `capabilities`, `degrade`, `max-request-bytes`, `context-limit`, `url-image-idle-timeout-seconds` (default 60). |
| `muse-profile` | off | Replay a recorded Muse Code wire profile: `enabled`, `profile`, `fill-defaults`, `keys.<client-key>`. |
| `mint-url`, `auth-url` | Meta's | Override Meta's key and login endpoints (testing only). |

## Limitations

- **Per-account proxies are not supported.** Accounts with `proxy_url` are refused and CPA skips to the next account. Use CPA's global `requests.proxy-url`, or separate CPA instances for different egress.
- **CPA Home mode** does not run plugin providers.
- In usage records the provider is `muse` instead of `meta`.

## Development

```sh
make test vet build
```

Pushing a `v*` tag runs CI: tests, builds for linux (glibc 2.17) and darwin on amd64 and arm64, a load check in the official CPA image, and a GitHub release with the zips and `checksums.txt` that the plugin store installs.

## License

[MIT](LICENSE)
