# Decisioni architetturali

## ADR 1 · Un binario, due profili

Il gateway Go conserva il comportamento personale di Claude/Codex e gestisce gli upstream HTTP nel profilo di produzione. Il profilo CLI ascolta su loopback. Il container usa il profilo produzione. Le credenziali dei provider API restano fuori dall'ambiente delle CLI, costruito con una lista esplicita di variabili. Il file TOML versionato ha un sottoinsieme dichiarato per mantenere il binario privo di dipendenze esterne. I vecchi file JSON restano utilizzabili.

## ADR 2 · Trasporto nativo e failover

Per i provider HTTP il gateway legge le informazioni necessarie a routing e policy, conserva i campi JSON dei provider e inoltra i frame SSE al completamento di ciascun evento. Sostituisce l'alias con il deployment, applica gli header di autenticazione e richiede usage nello streaming Chat. L'opzione max_tokens_field consente di tradurre il nome del limite di output. Le funzioni appartengono al client. Un retry o un fallback può avvenire prima dell'inizio di una risposta valida; dopo l'avvio dello stream gli eventi già consegnati appartengono al provider selezionato.

## ADR 3 · Attribuzione e contabilità

La chiave determina progetto e destinazioni autorizzate. La richiesta ha un ID e ogni tentativo un numero progressivo. Il registro append-only conserva inizio, esito, token disponibili e stima USD secondo il listino locale. Una prenotazione di budget sopravvive al crash. Il processo detiene un lock del sistema operativo per l'intera vita del registro. La persistenza su filesystem locale rende sufficiente un'istanza. La cache risposte è volatile, isolata per chiave e disabilitata di default.
