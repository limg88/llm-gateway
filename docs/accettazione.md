# Collaudo del 24 novembre 2026

Baseline del gateway Go: `ff28779`. Il collaudo di integrazione usa il codice del consumer e i protocolli HTTP reali verso provider simulati. I servizi esterni non hanno ricevuto richieste e le configurazioni attive dei consumer non sono state modificate.

| Criterio | Esito | Evidenza e perimetro |
|---|---|---|
| Migrazione di un progetto con URL e chiave | Verificato in integrazione | `TestRepoPilotConsumer` importa `analysis.ts` dal checkout di Repo Pilot e mantiene `claude/sonnet`. Tre richieste attraverso il gateway: richiesta tool, risultato della lettura Git, risposta a schema verificata con evidenza E1. |
| Fallback 5xx/429 | Verificato | `TestNativeFallbackAndLedger` simula separatamente 503 e 429, verifica due chiamate al primario e una al secondario. 400 e 401 fermano il routing. `TestNativePartialStreamAndDataPolicy` esclude un provider non autorizzato e controlla che uno stream parziale termini senza replay. |
| SSE end-to-end su due provider | Verificato con due provider mock; prova live aperta | `TestNativeIncremental` esercita Chat OpenAI e Messages Anthropic. Ogni upstream attende che il client abbia letto il primo frame prima di trasmettere il successivo. Il test confronta i frame ricevuti con quelli inviati. |
| Costi e token per progetto | Verificato | Il test di fallback confronta importo atteso, progetto e numero tentativo. `TestAnthropicPricing` separa input, output, letture cache e scritture a 5 minuti/1 ora. `TestUsageIsolationAndLedgerLock` verifica l'endpoint con due progetti e il rifiuto del secondo writer. |
| Test verdi e scansione segreti | Verificato sul checkout consegnato | 52 test Go di primo livello superati con race detector, consumer esterno incluso, zero test saltati. `go vet`, formato, `git diff --check` e Gitleaks 8.30.1 sul sorgente corrente. La scansione della storia precedente segnala tre occorrenze di credenziali sintetiche usate dai test di redazione nel commit di partenza; ispezionate, conservate nella storia originale. |
| Fonti e data delle misure di mercato | Documentato nelle Fasi 1–3 | Nel report ogni riga rimanda alle fonti. Sono indicate le finestre temporali, le misure parziali e quelle non reperite. [Analisi e decisione](https://chatgpt.com/library?file_id=libfile_81a5b2dcbfbc8191965892a8f92ad575&account_id=personal) |

## Come sono state effettuate le verifiche

Le porte TCP locali sono bloccate nell'ambiente di esecuzione. La suite ha usato HTTP/1.1 sopra connessioni full-duplex in memoria. Per Repo Pilot un socket locale già aperto ha collegato il trasporto di Node al server HTTP Go. Il provider era un mock HTTP indipendente. Il test consumer incluso nel repository usa direttamente il loopback quando l'ambiente lo consente. L'utente può ripeterlo indicando il checkout del consumer nel comando riportato nel README.

Verificati anche cache isolata per chiave, esclusione dei turni con tool, prenotazione di budget concorrente, recupero della riserva al riavvio e contratto Azure v1 per gli embeddings. Il binario è stato compilato per macOS arm64, Windows amd64 e Linux amd64. Eseguita la validazione di Docker Compose. Il daemon Docker non era accessibile per la build e l'avvio del container.

Per la messa in servizio rimangono la prova con credenziali reali sui modelli autorizzati, la riconciliazione con un addebito provider e l'avvio dello stack sull'host di destinazione. Il file di esempio instrada l'alias storico di Repo Pilot verso OpenAI. Va confermato che questa destinazione sia consentita per i dati del progetto prima del cutover.
