# Migrazione dei consumer

Avviare il gateway definitivo accanto all'istanza precedente. Mantenere URL, chiave, modello fisico ed encoder precedenti disponibili fino alla validazione specifica del consumer.

| Consumer | Modifica e verifica | Rollback |
|---|---|---|
| Repo Pilot | Impostare `REPO_PILOT_GATEWAY=http://localhost:8080/v1/chat/completions` e `REPO_PILOT_API_KEY` con la chiave assegnata. Lasciare invariato `REPO_PILOT_MODEL`. Configurare la destinazione di `claude/sonnet` sul server. Verificare una domanda che richieda una lettura del repository, citazione e risposta a schema entro il timeout di 30 secondi del client. | Ripristinare le due variabili e riavviare Repo Pilot. |
| Verbale | Riportare nel catalogo i nomi e i limiti in byte verificati sul vecchio endpoint models. Confrontare un report multiparte per lingua, numero di segmenti e completamento. | Ripristinare il profilo gateway precedente in Verbale. |
| Octobrief | Configurare la base senza `/v1` nel consumer. Mantenere modello embedding, digest, dimensione e normalizzazione dell'indice. Verificare Chat/tools, embedding e deadline. Il bridge Node con lease MCP e il metering per `X-Octobrief-Project` richiedono una verifica dedicata. Il nuovo gateway lega il progetto alla chiave. | Tenere disponibile il gateway Node per il percorso che dipende da lease o encoder specifici. |
| Octo | Il percorso Claude CLI è agentico e usa tool locali. Conservare l'integrazione esistente finché l'app ha un adapter HTTP con l'equivalente accesso alle evidenze. | Riattivare il runner CLI originale. |
| QA / SDK Anthropic | Configurare la radice del gateway e la chiave progetto. Provare uno stream Messages con `cache_control`, blocchi tool e ID correlato. | Ripristinare base e identità precedenti. |

## Dal Go e dal Node sperimentali

Il profilo personale legge la configurazione JSON Go. Sostituire chiavi letterali con riferimenti all'ambiente. Il profilo con CLI ascolta su loopback. In produzione usare il TOML di esempio, backend CLI esplicitamente vuoti, un nome progetto per credenziale e un volume locale durevole. Un'apertura del registro da un secondo processo deve fallire.

Dal Node tradurre `allowedBackends`, alias per app e classi dati rispettivamente in `backends`, `[api_keys.routes]` e `data_class/max_data_class`. Il nuovo HTTP nativo invia il primo evento quando arriva dal provider. I client che dipendono dalla validazione dell'intera risposta prima del primo byte devono gestire una fine stream con errore. Il percorso OpenAI-compatibile di Ollama usa il contesto configurato sul server/modello: riconvalidare il caso oltre contesto del vecchio adapter nativo.

Il registro precedente ha una semantica diversa. Conservare i vecchi segmenti in sola lettura con la loro versione di listino. Avviare il nuovo ledger su un file vuoto. Consultare i totali con la chiave del progetto su `GET /v1/usage`. Durante il cutover, evitare due gateway attivi sullo stesso file. Per archiviare il registro sospendere il processo e preservare tutte le righe della giornata UTC con budget attivi.

Le prove locali svolte riguardano Repo Pilot. La migrazione operativa di Verbale, Octobrief, Octo e QA e la dismissione delle istanze precedenti sono successive alla validazione dei relativi flussi.
