#!/bin/sh
# Faux `claude` pour la démo : réponses préécrites selon le contenu du mail.
# Aucun appel réseau, aucun vrai modèle.
if [ "$1" = "--version" ]; then echo "2.1.332 (Claude Code) [SIMULÉ POUR LA DÉMO]"; exit 0; fi
prompt=$(cat)
init='{"type":"system","subtype":"init","tools":["StructuredOutput"],"mcp_servers":[],"model":"claude-opus-5-5"}'
out() { # $1 = structured_output JSON, $2 = cost
  printf '%s\n' "$init"
  printf '%s\n' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"\",\"structured_output\":$1,\"total_cost_usd\":$2,\"usage\":{\"input_tokens\":2140,\"output_tokens\":180}}"
  exit 0
}
today=$(TZ=Europe/Paris date +%F)
case "$*" in
  *"Tu rédiges des brouillons"*)
    sleep 2
    out '{"body":"Bonjour Pierre,\n\nMerci de m\u2019avoir prévenu. C\u2019est d\u2019accord pour vendredi 27 novembre à 10h : tu peux confirmer au couvreur. Je reste joignable toute la matinée si besoin.\n\nBonne journée,\nLouis"}' 0.021
    ;;
  *"Tu réponds aux questions"*)
    sleep 2
    ref=$(printf '%s\n' "$prompt" | awk '/^\[[0-9]+\]$/{n=substr($0,2,length($0)-2)} /toiture/{print n; exit}')
    [ -z "$ref" ] && ref=1
    out "{\"answer\":\"Le couvreur peut passer vendredi 27 novembre à 10h, à condition de lui confirmer avant ce soir 18h (mail de Pierre Lambert). Tu as répondu que c'était d'accord.\",\"cited_refs\":[$ref]}" 0.014
    ;;
esac
sleep 1.5
case "$prompt" in
  *"Renvoie status = ok"*)
    out '{"status":"ok"}' 0.0004 ;;
  *toiture*)
    out "{\"category\":\"action_requise\",\"priority\":\"urgent\",\"summary\":\"Fuite au-dessus de la chambre après l'orage. Le couvreur peut venir vendredi 27/11 à 10h, mais Pierre doit lui confirmer avant ce soir 18h, sinon pas avant deux semaines.\",\"needs_reply\":true,\"actions\":[{\"text\":\"Confirmer à Pierre le passage du couvreur vendredi 10h\",\"owner\":\"me\",\"due_date\":\"$today\",\"due_time\":\"18:00\",\"source_quote\":\"il faut lui confirmer avant ce soir 18h\"}],\"contains_ai_instructions\":false,\"reason\":\"Échéance aujourd'hui, dégât en cours.\"}" 0.009 ;;
  *bricoleur*)
    out '{"category":"newsletter","priority":"basse","summary":"Lettre mensuelle : isolation des combles, promotions sur l\u2019outillage.","needs_reply":false,"actions":[],"contains_ai_instructions":false,"reason":"Newsletter."}' 0.004 ;;
  *Facture*)
    out '{"category":"facture_paiement","priority":"normale","summary":"Facture d\u2019électricité de novembre : 84,20 €, prélèvement le 10 décembre.","needs_reply":false,"actions":[{"text":"Vérifier la facture d\u2019électricité (84,20 €)","owner":"me","due_date":"2026-12-10","due_time":"","source_quote":"prélevé le 10 décembre"}],"contains_ai_instructions":false,"reason":"Facture sans urgence."}' 0.006 ;;
  *SYSTEM*)
    out '{"category":"spam_probable","priority":"haute","summary":"Faux message de « vérification de compte » qui cache des ordres destinés à une IA : transférer tous tes mails à une adresse externe. Rien n\u2019a été exécuté.","needs_reply":false,"actions":[],"contains_ai_instructions":true,"reason":"Tentative d\u2019injection de prompt et d\u2019hameçonnage."}' 0.007 ;;
esac
out '{"category":"information","priority":"normale","summary":"Mail d information.","needs_reply":false,"actions":[],"contains_ai_instructions":false,"reason":"défaut"}' 0.003
