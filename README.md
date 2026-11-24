# llm-gateway

Un binario Go per i client OpenAI Chat Completions e Anthropic Messages. Nel profilo **production** inoltra le richieste ai provider HTTP e attribuisce i tentativi a un progetto. Nel profilo **personal** espone anche le CLI Claude e Codex installate sul computer. Un solo processo può utilizzare lo stesso registro locale.

## Quick start con Docker Compose

Dalla directory del repository, con Docker Compose disponibile e le credenziali dei provider già caricate nell'ambiente. L'esempio usa sia `OPENAI_API_KEY` sia `ANTHROPIC_API_KEY`. Per usarne uno solo, copiare il TOML, rimuovere l'altro upstream e i riferimenti alle sue rotte, quindi impostare `GATEWAY_CONFIG` sul file personalizzato.

```sh
export GATEWAY_PROJECT_KEY="$(openssl rand -hex 32)"
docker compose up --build -d
curl -fsS http://localhost:8080/health
curl -fsS -H "Authorization: Bearer $GATEWAY_PROJECT_KEY" http://localhost:8080/v1/models
```

Conservare la chiave generata nel proprio secret manager. Su PowerShell impostare le stesse variabili in `$env:` e usare `curl.exe`. Il servizio è pubblicato sul loopback dell'host. Per un host aziendale terminare TLS al reverse proxy. Il volume Compose `usage` contiene il registro dei tentativi. L'immagine di produzione usa un utente non privilegiato.

Per il binario nativo eseguire `go build -o llm-gateway .` e `./llm-gateway -config gateway.example.toml`. Il profilo personale si avvia senza file di configurazione su loopback; le CLI devono avere già effettuato il login sull'host. Le immagini di produzione contengono solo il gateway e i certificati necessari alle connessioni HTTPS. [Protocollo personale e opzioni legacy](docs/profilo-personale.md)

## Client e rotte

| Endpoint | Uso |
|---|---|
| `POST /v1/chat/completions` | Chat nativa OpenAI, Azure v1 o server compatibile. Tool e schemi vengono inoltrati al modello. SSE se `stream: true`. |
| `POST /v1/messages` | Anthropic nativo, con header versione e beta, blocchi di contenuto, tool, thinking e prompt caching. Auth client Bearer o `x-api-key`. |
| `POST /v1/embeddings` | Endpoint OpenAI-compatibile o Azure per un encoder configurato. |
| `POST /v1/responses` | Nativo stateless su provider OpenAI-compatibili che lo implementano. Le conversazioni continuabili del profilo CLI restano nella memoria del gateway. |
| `GET /v1/models` | Catalogo filtrato per chiave e classe dati, con alias e metadati di discovery configurabili. |
| `GET /v1/usage` | Totali del progetto associato alla chiave quando il registro è configurato. Importi USD stimati, token per categoria, cache hit e tentativi non valorizzati o aperti. |
| `GET /health`, `GET /metrics` | Stato del processo e metriche Prometheus. Nel TOML di esempio le metriche richiedono la chiave progetto. |

Per usare l'SDK OpenAI impostare `base_url=http://localhost:8080/v1`. Per l'SDK Anthropic usare `base_url=http://localhost:8080` e l'alias `native-sonnet`. Con Repo Pilot passare l'URL completo `http://localhost:8080/v1/chat/completions`. Nell'esempio il suo alias legacy `claude/sonnet` seleziona **OpenAI GPT-5.4 mini**: verificare la destinazione autorizzata prima di inviare dati. L'endpoint Messages seleziona un upstream con `protocol = "anthropic"`. Gli alias con fallback devono elencare destinazioni capaci di eseguire lo stesso contratto. Il gateway conserva gli ID e gli argomenti dei tool; è il consumer a eseguirli.

## Configurazione

[Configurazione completa di partenza](gateway.example.toml). Il profilo TOML supporta chiavi semplici o tra virgolette doppie, stringhe base, numeri decimali, booleani, array su una riga, tabelle e array di tabelle. Ogni valore occupa una riga. Le chiavi sconosciute interrompono l'avvio. I file JSON legacy sono ancora leggibili. Applicare le modifiche riavviando l'istanza. Con Compose usare `docker compose up -d --force-recreate` dopo aver cambiato l'ambiente.

| Impostazione | Comportamento |
|---|---|
| `[[api_keys]]` | `key_env` identifica una credenziale del gateway; `project` fissa l'attribuzione. `rpm` limita le richieste per chiave. `backends` limita le destinazioni ammesse. Per produzione servono chiavi distinte da quelle upstream. |
| `[models]`, `[api_keys.routes]` | Elenchi ordinati di `provider/modello`; la tabella della chiave prevale sul catalogo globale. Le stesse autorizzazioni si applicano a ogni destinazione. |
| `data_class`, `max_data_class` | Su chiave e upstream. Ordine crescente: `synthetic`, `pseudonymized`, `client`. Una chiave classificata può usare solo upstream HTTP con classe sufficiente. Configurare le classi approvate per i progetti aziendali. |
| `[[upstreams]]` | `name`, `protocol`, `base_url`, `models`, `api_key_env`. Per Azure usare `azure`, la radice `https://<risorsa>.openai.azure.com/openai/v1` e il nome del deployment. Per Ollama/LM Studio usare `openai` e il loro endpoint `/v1`. Da Docker Desktop, un modello sull'host è raggiungibile tramite `host.docker.internal`. |
| `retries`, `request_timeout` | Da 0 a 4 retry per destinazione, backoff con jitter e deadline comune. Ammessi 429, 500, 502, 503, 504, 529 ed errori di trasporto. Un Retry-After oltre il tempo residuo evita un nuovo tentativo sullo stesso provider. Un'interruzione dopo un evento SSE termina lo stream. |
| `[prices."provider/modello"]` | USD per milione di token. `input`, `output`, `cache_read`, `cache_write` (5 minuti), `cache_write_1h`. Versionare il listino con `price_version`. Le stime riguardano token testuali; eventuali tariffe contrattuali, regionali e accessorie vanno verificate separatamente. Una tariffa o un dato di usage mancante dà costo nullo (JSON `null`). |
| `[budgets."progetto"]` | `daily_usd` e `reserve_usd` per tentativo. Il controllo serializza le ammissioni sul processo, riserva prima dell'invio e regola il saldo sul costo conosciuto. Una riserva resta impegnata per l'intera giornata UTC se manca l'usage. Il budget limita le nuove ammissioni; una risposta più costosa della propria riserva può superarlo. Ogni retry e fallback richiede una nuova riserva. |
| `cache_ttl_seconds` | 0 disabilita la cache risposte. Se attiva, al massimo 128 risposte da 64 KiB, in memoria, chiave isolata per credenziale, destinazione e payload. Solo Chat testuale non streaming con temperatura 0 esplicita e senza strumenti. Un hit ha costo upstream zero e header `X-Gateway-Cache: hit`. La prompt cache Anthropic è gestita dal provider attraverso `cache_control`. |
| `[discovery."alias"]` | `input_limit_bytes`, `report_input_limit_bytes`, `provider_type` per il frazionamento dei report Verbale. |

## Collaudo e migrazione

```sh
go vet ./...
go test -race -count=1 ./...
REPO_PILOT_DIR=/percorso/repo-pilot go test -run '^TestRepoPilotConsumer$' -count=1
```

Il test consumer richiede Node con supporto TypeScript e un checkout Git del gateway. Usa il modulo originale di Repo Pilot, un provider mock e il commit corrente per verificare l'evidenza letta. In CI senza porte TCP, `GATEWAY_TEST_PIPE=1` collega client e server Go via HTTP su pipe. La suite standard usa TCP su loopback.

[Guida di migrazione](docs/migrazione.md) · [Decisioni architetturali](docs/decisioni.md) · [Esiti e limiti del collaudo](docs/accettazione.md)
