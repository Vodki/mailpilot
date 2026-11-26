# mailpilot

Agent personnel qui surveille une boîte OVH Email Pro en IMAP, résume chaque
mail avec un LLM (`claude -p`), notifie sur Telegram, crée des rappels Todoist
et prépare des réponses validées avant tout envoi.

## Ce que fait mailpilot

- **Surveillance** : connexion IMAP permanente (IDLE, rafraîchie toutes les
  25 min), ou relève toutes les 90 s si le serveur n'annonce pas IDLE. La
  boîte est lue en `BODY.PEEK[]` : l'état lu ou non lu ne change jamais.
- **Tri** : `rules.yaml` d'abord (ignorer, digest seulement, priorité…),
  puis le LLM, qui rend un JSON validé par schéma : catégorie, priorité,
  résumé, actions avec citation du mail.
- **Telegram** : notification immédiate de ce qui est urgent ou important,
  le reste dans les digests de 08:30 et 18:00.
- **Todoist** : les actions datées deviennent des tâches, les autres sont
  proposées avec un bouton (/rappels).
- **Réponses** : ✍️ Brouillon rédige une réponse, la range dans tes
  Brouillons et propose Envoyer, Modifier, Régénérer ou Annuler. L'envoi
  demande une seconde confirmation et reste impossible tant que
  `smtp.send_enabled` vaut `false`.
- **Questions** : `/ask quand passe le couvreur ?` cherche dans les mails
  indexés (plein texte) et répond en citant ses sources.
- **Garde-fous** : `mailpilot check`, alerte Telegram si IMAP décroche,
  corps des mails effacés après 30 jours (résumés conservés).

### Commandes Telegram

| Commande | Effet |
| --- | --- |
| `/status` | état d'IMAP, pause, envoi, coût LLM du jour, rappels, index |
| `/digest` | digest immédiat |
| `/ask question` | recherche dans les mails, réponse avec sources |
| `/pause`, `/resume` | coupe et rétablit notifications et envois |
| `/mute adresse\|domaine [jours]` | expéditeur envoyé au digest seulement (aussi en réponse à une notification) |
| `/unmute adresse\|domaine` | annule une sourdine |
| `/rappels` | actions en attente de confirmation pour Todoist |
| `/reload` | relit `rules.yaml`, `profile.md` et les prompts |
| `/annuler` | abandonne la modification d'un brouillon en cours |

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

## Installation pas à pas

### 1. Le bot Telegram

1. Dans Telegram, ouvre une conversation avec **@BotFather**, envoie
   `/newbot`, choisis un nom puis un identifiant finissant par `bot`.
2. BotFather répond avec un jeton du type `123456789:AA…` : c'est
   `MAILPILOT_TELEGRAM_TOKEN`. Garde-le secret ; `/revoke` chez BotFather en
   crée un nouveau si besoin.
3. Facultatif mais conseillé : chez BotFather, `/setjoingroups` puis
   *Disable*, pour que personne ne puisse ajouter le bot à un groupe.
   mailpilot refuse de toute façon les groupes.

### 2. Ton `chat_id`

mailpilot ne parle qu'à un seul compte, celui dont le `chat_id` est dans
`config.yaml`. Pour le trouver, mailpilot étant arrêté :

1. Envoie un message quelconque à ton bot (bouton *Démarrer*).
2. Ouvre dans un navigateur
   `https://api.telegram.org/bot<TON_JETON>/getUpdates`.
3. Repère `"chat":{"id":123456789,…}` : ce nombre est ton `chat_id`.

Reporte-le dans `telegram.chat_id`. Tout message venant d'un autre compte est
ignoré et journalisé (identifiant seulement).

### 3. Todoist (facultatif)

1. Todoist, *Paramètres > Intégrations > Développeur* : copie le jeton
   d'API dans `MAILPILOT_TODOIST_TOKEN`.
2. Dans `config.yaml`, `todoist.enabled: true`. Pour ranger les tâches dans
   un projet précis, ouvre-le dans Todoist web : l'identifiant est la fin de
   l'adresse (`…/app/project/nom-6Jf8VQXxpwv56VQ7` donne
   `6Jf8VQXxpwv56VQ7`) ; mets-le dans `todoist.project_id`.
3. `mode_dated: auto` crée directement les tâches datées ; `confirm` les
   propose avec un bouton ; `off` les ignore. Une action dont la citation
   n'est pas retrouvée dans le mail, ou un mail suspect, passe toujours par
   la confirmation.

### 4. OVH Email Pro

Dans l'espace client OVHcloud, *Web Cloud > Email Pro > ton service*,
onglet *Informations générales* : le nom du serveur est de la forme
`proX.mail.ovh.net` (X dépend de ton service).

| Réglage | Valeur |
| --- | --- |
| `imap.host` / `smtp.host` | `proX.mail.ovh.net` |
| `imap.port`, `imap.security` | `993`, `tls` |
| `smtp.port`, `smtp.security` | `587`, `starttls` |
| `imap.username` | ton adresse complète |
| `MAILPILOT_IMAP_PASSWORD` | le mot de passe de la boîte |

Lance ensuite `mailpilot check -only imap,smtp`. S'il signale que le
serveur n'annonce pas SPECIAL-USE, renseigne `imap.drafts_folder` et
`imap.sent_folder` avec les noms affichés par `check` (souvent `Drafts` et
`Sent`, parfois `INBOX.Drafts`). Ajoute tes alias dans `self_aliases`.

L'envoi reste coupé tant que `smtp.send_enabled` vaut `false`. Une fois les
brouillons vérifiés quelques jours, passe-le à `true` : chaque envoi
demandera quand même ✉️ Envoyer puis ✅ Confirmer, avec les destinataires
calculés depuis les en-têtes du fil, et au plus `max_sends_per_hour` envois
par heure. `/pause` coupe tout à distance.

### 5. Le LLM

Sur ta machine, avec un abonnement Claude : `claude setup-token`, puis copie
le jeton dans `CLAUDE_CODE_OAUTH_TOKEN`. Une clé `ANTHROPIC_API_KEY`
fonctionne aussi (facturation à l'usage). Les modèles, l'effort et le budget
maximal par appel se règlent dans `llm.tasks`. `/status` affiche le coût du
jour.

### 6. Ton profil et tes règles

- `profile.md` : qui tu es, tes clients, ton ton, ce qui est urgent pour
  toi. Il est injecté dans chaque analyse et chaque brouillon (32 Ko
  maximum).
- `rules.yaml` : règles déterministes appliquées avant le LLM (voir
  l'exemple commenté dans `config.example/rules.yaml`).

Les deux se rechargent sans redémarrer : `/reload` sur Telegram ou
`docker compose -f deploy/docker-compose.yml kill -s HUP mailpilot`. Une erreur garde l'ancienne version
et te la signale.

## Déploiement sur un VPS

Prérequis : un petit VPS (1 vCPU, 1 Go de RAM suffisent) avec Docker et le
plugin compose. Aucun port entrant n'est nécessaire : IMAP, SMTP, Telegram
(long polling) et le LLM sont des connexions sortantes. Tu peux fermer tout
sauf SSH.

```sh
git clone https://github.com/Vodki/mailpilot.git && cd mailpilot
cp .env.example .env && chmod 600 .env    # remplir les secrets
cp -r config.example config               # adapter config/config.yaml
docker compose -f deploy/docker-compose.yml build
docker compose -f deploy/docker-compose.yml run --rm mailpilot check
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml logs -f
```

Au premier démarrage, les mails des 7 derniers jours sont importés en
silence (indexés pour `/ask`, sans notification), puis le bot annonce son
démarrage sur Telegram.

Mise à jour : `git pull`, puis `build` et `up -d` à nouveau. Les
migrations de la base s'appliquent au démarrage ; une base plus récente que
le binaire est refusée. Sauvegarde : le volume `mailpilot-data` contient
toute la base (`/data/mailpilot.db`).

Pour des secrets en fichiers plutôt que dans `.env` (Docker secrets,
gestionnaire de mots de passe), chaque variable accepte la variante
`NOM_FILE=/chemin`.

## Configuration

| Fichier | Contenu |
| --- | --- |
| `config.yaml` | réglages non secrets ; toute clé inconnue est refusée (`config.example/config.yaml`) |
| `.env` | secrets (`.env.example`) ; chaque variable accepte aussi `NOM_FILE=/chemin` |
| `rules.yaml` | règles de pré-filtrage, rechargées par SIGHUP ou /reload |
| `profile.md` | ton profil, injecté dans le prompt système |

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

## Image Docker

L'image est construite en plusieurs étapes et tourne sur
`distroless/cc-debian12:nonroot`, système de fichiers en lecture seule,
volumes `/data` et `/config`. Claude Code y est installé à une version figée
(`CLAUDE_CODE_VERSION` dans `deploy/Dockerfile`).
