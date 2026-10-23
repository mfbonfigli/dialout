# dialout

Let an AI agent make phone calls. dialout is an MCP server (and a command-line tool) that places an outbound call through Twilio, lets a speech-to-speech voice agent hold the conversation, and returns the transcript and the outcome.

- **Swappable voice engines:** Amazon Nova 2 Sonic (default), ElevenLabs Agents, OpenAI Realtime.
- **No inbound ports:** Twilio's audio arrives through a Cloudflare tunnel that runs only during a call. It's a quick tunnel by default (no account or domain needed), or your own hostname.
- **A policy in front of every call:** a kill switch, allowed countries and number types, an allowlist (or open dialling with calling hours and per-number limits), a blocklist, rate limits and size limits, all checked by the server and not by the agent that asks.
- **Transcripts always, audio only if you want it.** They're stored in a folder or an S3 bucket, uploaded during the call, and recovered after a crash.
- **Your prompt:** a built-in Italian prompt, replaceable block by block with a template file.
- **One small image:** the static binary plus cloudflared, non-root, read-only, MCP over HTTP with a bearer token.

> **Status: early.** The call engine has been used on real calls. The MCP server, the YAML configuration and the prompt file are new and tested only offline. By default only **Italian** fixed and mobile numbers on the allowlist can be called, and the built-in prompt is Italian.

## How a call goes

1. An MCP client calls `place_call` with a number (or a label) and a brief. dialout checks the policy and returns a `call_id` at once.
2. It starts cloudflared, waits until the tunnel answers, then creates the Twilio call with inline TwiML `<Connect><Stream>`.
3. When the callee answers, Twilio streams the audio over a WebSocket. dialout checks the signature and the call, and bridges the audio to the voice engine.
4. The agent talks, then calls `report_outcome` and `end_call`. dialout hangs up once the goodbye has played. Time limits, cues and barge-in are handled along the way.
5. `get_call` returns the status, the transcript and the agent's outcome. The outcome is a hint: judge from the transcript.

## Requirements

- **Twilio:** a paid account with the Primary Customer Profile approved. Trial accounts can't stream audio, and the API refuses calls until the profile is approved. You also need a caller ID: a verified number of yours, or one bought on the account. Restrict the account's voice geographic permissions to the countries you call.
- **A voice engine:** AWS credentials allowed to invoke Nova 2 Sonic (`eu-north-1` in the EU), or an ElevenLabs API key and agent, or an OpenAI API key.
- **cloudflared** in `PATH` for the command-line tool. It's included in the image.

## Quick start: one call from your machine

```bash
mkdir -p ~/.config/dialout
cp examples/dialout.example.yaml ~/.config/dialout/dialout.yaml
cp examples/secrets.env.example ~/.config/dialout/secrets.env
# edit both: owner, caller ID, allowlist, keys
go run ./cmd/dialout call -to "Marco (mobile)" -objective "Chiedigli a che ora arriva stasera."
```

The command shows a summary, asks for confirmation, prints the conversation as it happens, and stores the transcript under `~/.dialout/calls`. Run `dialout call -h` for the flags: `-callee`, `-kind`, `-engine`, `-voice`, `-record`, `-prompt` and more.

## MCP

| Tool | Does |
|---|---|
| `list_numbers` | The numbers that may be called, whether calls are enabled, the limits, the calls left |
| `place_call` | Place a call: `to`, `objective`, and optionally `callee`, `kind` (`business` or `personal`), `opening`, `limits`, `context`, `max_minutes` (only shorter than the policy's). Returns the `call_id` at once. The voice is the owner's setting, not the caller's |
| `get_call` | Status, outcome and transcript; `wait_seconds` (≤ 120) waits for the end |
| `hang_up` | Hang up, or cancel while it rings |

**Over HTTP** (`dialout serve`): Streamable HTTP at `server.mcp_listen`, path `/mcp`. Every request needs `Authorization: Bearer <server.mcp_token>`, a token of at least 32 characters (`openssl rand -hex 32`). Cross-origin browser requests are refused. For example, with Claude Code:

```bash
claude mcp add --transport http dialout http://127.0.0.1:8080/mcp --header "Authorization: Bearer $DIALOUT_MCP_TOKEN"
```

or in an agent's YAML configuration:

```yaml
mcp_servers:
  phone:
    url: "http://dialout:8080/mcp"
    headers:
      Authorization: "Bearer ${DIALOUT_MCP_TOKEN}"
    timeout: 150          # longer than get_call's 120 s wait
```

**Over stdio** (`dialout mcp`): for a client that starts dialout itself, e.g. `claude mcp add dialout -- dialout mcp -config /path/to/dialout.yaml`. No token is needed: the client owns the process.

Writing a good brief: from the agent's point of view, in the language of the call. `objective` holds what to obtain, with the preferred option. `limits` holds the margins the agent may use but never says. `context` holds the facts it may share. `callee` and `kind` say who answers. An example for a pizzeria:

```json
{"to": "Pizzeria", "callee": "la pizzeria Da Mario", "kind": "business",
 "opening": "Buonasera, vorrei ordinare una pizza da asporto.",
 "objective": "Una pizza margherita da asporto, da ritirare alle 22.",
 "limits": "Va bene dalle 21:30 alle 23. Fino a 20 euro. Niente pagamenti anticipati.",
 "context": "La prenotazione è a nome di Marco."}
```

## Configuration

One YAML file, `dialout.yaml`. It's found from `-config`, else `$DIALOUT_CONFIG`, else `dialout/dialout.yaml` in the user's configuration folder, else `/etc/dialout/dialout.yaml`. [examples/dialout.example.yaml](examples/dialout.example.yaml) lists every key.

- **Secrets stay out of the file.** It references them as `${VAR}`, filled from the environment or from `secrets_file` (a `KEY=VALUE` file; the environment wins). `$$` is a literal `$`.
- **Strict:** an unknown key or a wrong type is an error. Everything is checked at start-up, the prompt file included.
- **Reloaded before every call:** the policy and the prompt file, so you can change the allowlist or tune the prompt without a restart. Other settings apply at the next start.
- **Data:** the spool, the stored calls and the server's history go under `$DIALOUT_DATA` (`/data` in the image), else `~/.dialout`, unless `storage` says otherwise.

## Who can be called

The policy is checked on every `place_call`, and the configuration's policy section is re-read each time.

1. **Dialable numbers, in every mode:** `countries` (ISO codes, `[IT]` by default) and `number_types` (`fixed`, `mobile`, and optionally `toll_free`). Numbers are classified with [libphonenumber](https://github.com/nyaruka/phonenumbers)'s numbering plans:
   - premium-rate, shared-cost, VoIP, personal, pager and unclassifiable numbers are never called;
   - emergency and short numbers aren't valid full numbers, so they're refused too;
   - countries are matched by region, not by calling code: `+1 876` is Jamaica, so `countries: [US, CA]` doesn't reach it;
   - where the numbering plan can't tell fixed from mobile (the US, Canada), a number passes if either type is allowed.
2. **The blocklist:** numbers or prefixes, in international form, never called in any mode.
3. **The mode:**
   - `allowlist` (default): only the listed numbers.
   - `open`: any dialable number. The allowlist still gives labels and default callee and kind. Numbers *not* on the allowlist also get calling `hours` (default `09:00-21:00` in `timezone`) and `per_number_per_day` (default 2), so a confused agent can't ring a stranger at night or call the same person over and over.
4. **The limits:** one call at a time, `per_hour`, `per_day`, call length and brief sizes.

Open mode removes the main guard: an agent manipulated by something it read could call anyone in the allowed countries. Keep the MCP client's approval for every call, Twilio's geographic permissions set to the same countries, and a small prepaid balance. When calling strangers, consider having the agent say up front that it's an assistant, in the prompt file. Disclosure and recording rules vary by country.

## Prompt file

`prompt.file` points to a Go `text/template` file of named blocks. The blocks you leave out come from the [built-in file](prompts/builtin.tmpl). [examples/prompt-en.tmpl](examples/prompt-en.tmpl) is a complete English prompt.

| Block | Used for |
|---|---|
| `system` (required) | The system prompt |
| `closing.tools_then_goodbye`, `closing.with_goodbye` | The closing sentence, `.Closing`: Sonic calls the tools before the goodbye, the others with it |
| `cue.opening`, `cue.wrap_up`, `cue.final` | Lines sent while the call runs: silence after the answer, 30 s before the limit, at the limit |
| `tool.next.end_call`, `tool.next.goodbye` | Sonic's tool results: the next step. Keep them in the call's language |
| `tool.report_outcome[.result/.summary/.details]`, `tool.end_call[.reason]` | Tool descriptions, English by default |

Variables: `.Owner`, `.Female` (the voice's grammatical gender), `.Callee`, `.Kind`, `.Label`, `.Opening`, `.Objective`, `.Limits`, `.Context`, `.Now` and `.Closing`. `{{weekday .Now "Sunday Monday …"}}` and `{{month .Now "January …"}}` give day and month names in any language. Every block is test-rendered at load time, so a typo fails at start-up, not on a call. The brief is inserted as data and never evaluated.

## Docker

```bash
docker build -t dialout .
docker compose -f examples/compose.yaml up -d
```

The image runs `dialout serve` as a non-root user on a read-only filesystem, with cloudflared pinned by checksum. It expects `/config/dialout.yaml` and keeps its data in the `/data` volume. Keep the MCP port private:
- publish it only on `127.0.0.1`, or not at all;
- or put dialout on a Docker network shared only with the agent that uses it;
- and always keep the token.

The image doesn't need AWS credentials unless you use Sonic or S3. In that case, mount a credentials **folder** read-only and set `AWS_PROFILE`. Mount the folder, not the file: tools that rotate the file replace it, and a single-file mount keeps showing the old one. Containers can't reach an EC2 instance role when the instance metadata service is limited to one hop.

## Security model

- The policy inside the server is the guard: the kill switch, the allowed countries and number types (premium, short and emergency numbers are refused even if listed), the blocklist, the allowlist or open mode's hours and per-number limits, rate limits and one call at a time. The agent that asks can't change it, and the token only stops other software from using it.
- The transcript is what a third party said. MCP clients should treat it as data, never as instructions.
- The agent never gives payment details and never pays: the built-in prompt says so, and nothing in dialout can pay.
- cloudflared gets an allowlisted environment (no keys). The Twilio WebSocket is accepted only with a one-time token, a valid signature and the expected call.
- Limit damage outside dialout too: Twilio's geographic permissions, a small prepaid balance with auto-recharge off.

## ElevenLabs agent set-up

1. Create an agent in the target language with an **empty first message**, so the agent waits for the callee's "hello".
2. Set audio in and out to **μ-law 8000 Hz**.
3. Under Security, enable the **System prompt** override, plus Voice or LLM if you set them. Then **publish** the agent.
4. Add a client tool `report_outcome` with "wait for response" on, using the fields of the built-in description. Keep ElevenLabs' built-in **End call** tool.

## Development

```bash
go test ./...
SONIC_LIVE=1 go test ./internal/sonic -run TestLive -v   # talks to Nova 2 Sonic: costs money
```

## License

To be chosen.
