// Package authcompat holds tests proving that credential bearer tokens produced
// by doltlite (the C client, src/doltlite_creds.c) validate under the same JWT
// rules DoltHub's remote API enforces (go-jose EdDSA, issuer/audience/subject,
// and the dolt_token_version header). It has no runtime code.
package authcompat
