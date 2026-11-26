# Tests de bout en bout et vidéo démo

Un vrai mailpilot, GreenMail, Chromium (Playwright), avec Telegram, Todoist et
`claude` simulés (`fakes.py`, `fake-claude.sh`) : aucun vrai identifiant,
aucun vrai modèle.

```sh
make test-env-up                           # GreenMail (deploy/docker-compose.test.yml)
npm install --prefix tools/e2e
npx --prefix tools/e2e playwright install chromium
tools/e2e/run.sh                           # les tests (e2e.js), aussi lancés par la CI
tools/e2e/run.sh video ./rec               # filme la démo (video.js) dans ./rec
```

`run.sh` vide la boîte de test, compile mailpilot, le lance avec
`config.yaml` (envoi activé : GreenMail ne livre qu'à ses propres comptes)
et dépose les mails de `mailbox.py` pendant le test.
