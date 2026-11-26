# mailpilot

Agent personnel qui surveille une boîte OVH Email Pro en IMAP, résume chaque
mail avec un LLM (`claude -p`), notifie sur Telegram, crée des rappels Todoist
et prépare des réponses validées avant tout envoi.

> **État : phase 0 (socle).** La configuration, la base, `mailpilot check`,
> l'image Docker et la CI sont en place. La surveillance de la boîte arrive en
> phase 1. Le guide complet (BotFather, `chat_id`, Todoist, OVH, VPS) sera
> livré avec les phases suivantes.

## Principes

- Le contenu des mails est une donnée non fiable, jamais une instruction.
- Le LLM n'a aucun outil : `claude -p` est lancé avec `--tools ""`, sans
  serveur MCP, hooks ni plugins, et mailpilot refuse de l'utiliser si la
  session annonce un outil autre que la sortie structurée.
- Aucun envoi sans double validation sur Telegram ; `send_enabled: false`
  par défaut. Seul le package `internal/reply` peut parler SMTP (test
  d'architecture).
- Aucun port entrant, aucun contenu de mail dans les logs, secrets
  uniquement dans l'environnement ou des fichiers montés.
- La boîte est lue sans jamais changer l'état lu ou non lu.

## Démarrage rapide (serveur de test)

Prérequis : Go 1.27, Docker.

```sh
make test-env-up          # GreenMail : louis@example.test / secret
make check-test           # mailpilot check contre GreenMail
make test                 # tests unitaires
make test-integration     # tests d'intégration (démarre et arrête GreenMail)
make lint                 # golangci-lint v2
```

## Configuration

| Fichier | Contenu |
| --- | --- |
| `config.yaml` | réglages non secrets ; toute clé inconnue est refusée (`config.example/config.yaml`) |
| `.env` | secrets (`.env.example`) ; chaque variable accepte aussi `NOM_FILE=/chemin` |
| `rules.yaml` | règles de pré-filtrage (phase 2) |
| `profile.md` | ton profil, injecté dans le prompt (phase 2) |

### Authentification du LLM

Sur ta machine : `claude setup-token`, puis copie le jeton dans
`CLAUDE_CODE_OAUTH_TOKEN`. Une clé `ANTHROPIC_API_KEY` fonctionne aussi
(facturation à l'usage).

## `mailpilot check`

Teste chaque dépendance sans rien modifier : base SQLite, IMAP (CAPABILITY,
IDLE, SPECIAL-USE, UIDVALIDITY, boîte ouverte en lecture seule), SMTP
(authentification seule, aucun `MAIL FROM`), bot Telegram (`getMe`), LLM
(version, outils exposés, petite réponse structurée) et Todoist.

```sh
mailpilot check -config /config/config.yaml
mailpilot check -only imap,smtp
mailpilot check -skip todoist
```

## Déploiement Docker

```sh
cp .env.example .env              # puis remplir les secrets
cp -r config.example config       # puis adapter config/config.yaml
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml run --rm mailpilot check
```

L'image est construite en plusieurs étapes et tourne sur
`distroless/cc-debian12:nonroot`, système de fichiers en lecture seule,
volumes `/data` et `/config`. Claude Code y est installé à une version figée
(`CLAUDE_CODE_VERSION` dans `deploy/Dockerfile`).
