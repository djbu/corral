# Evidencia M7B — release privada reproducible

**Fecha:** 2026-08-09 (America/Bogota) / 2026-08-10 UTC  
**PR:** [`djbu/corral#2`](https://github.com/djbu/corral/pull/2)  
**SHA validado:** `0b0b61f58162e9032390685595e1669af665ac39`  
**Tag temporal retirado:** `v0.7.0-m7b.1`

## Gates locales

- GoReleaser `v2.17.1`: configuración válida.
- Actionlint `v1.7.12`: los tres workflows válidos.
- `bash -n scripts/release/*.sh`: verde.
- `node --check internal/api/dashboard/app.js`: verde.
- `go test ./...`: verde.
- `go vet ./...`: verde.
- tag ligero sintético: rechazado por `verify-tag.sh`.
- validador de CI: aceptó un SHA con los dos checks requeridos.

El gate de reproducibilidad clonó dos veces el tag en rutas absolutas distintas.
Los dos builds produjeron el mismo inventario y el mismo SHA-256 para cada uno
de los cuatro archives. Cada build verificó también los cuatro SBOM SPDX, el
archivo agregado de checksums, el inventario del tar y la metadata embebida de
versión, commit completo y API 1.

## Gate GitHub real

El tag anotado temporal `v0.7.0-m7b.1` apuntó al SHA anterior. El workflow
[`release` #31352750674](https://github.com/djbu/corral/actions/runs/31352750674)
completó:

1. validación de tag anotado y checkout exacto;
2. comprobación de `test (macos-latest)` y `test (ubuntu-latest)`;
3. instalación de Go 1.26.5 y Syft 1.50.0;
4. build de draft con GoReleaser 2.17.1;
5. verificación de los cuatro artefactos;
6. publicación del draft verificado como prerelease privada.

La release publicada contenía cuatro archives, cuatro SBOM, checksums y
`metadata.json`. Se descargó completa mediante una sesión autenticada de
GitHub CLI y `verify-artifacts.sh` volvió a pasar fuera del runner.

## Limpieza y resultado

Después de verificar la descarga se eliminaron exclusivamente la release y el
tag remoto temporal. También se retiró el tag local. La consulta posterior
confirmó que ninguno seguía existiendo. Los tags estables `v0.1.0`–`v0.6.0`
no se modificaron.

M7B no publica `v0.7.0`: ese tag queda reservado para el cierre de los pasos
50–54 de M7.
