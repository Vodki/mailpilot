# Démo filmée

Rejoue la démo de bout en bout contre GreenMail, avec Telegram, Todoist et
`claude` simulés (aucun vrai identifiant).

```sh
make build && make test-env-up          # GreenMail neuf : relancer le conteneur entre deux prises
go run ./tools/demo &                   # page de démo sur http://localhost:8080
node tools/demo/record.js ./rec         # filme la page (Playwright), webm dans ./rec
ffmpeg -i rec/*.webm -c:v libx264 -crf 22 -pix_fmt yuv420p mailpilot-demo.mp4
```
