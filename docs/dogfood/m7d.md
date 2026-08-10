# Evidencia M7D — instalación privada verificable

**Fecha:** 2026-08-10 (America/Bogota)
**Alcance:** paso 52 de M7

## Alcance validado

- detección nativa de macOS/Linux y amd64/arm64;
- versión explícita y prefijo absoluto de usuario, sin `sudo` ni root;
- descarga de `djbu/corral` con `gh` y token sólo por entorno;
- instalación offline desde archive + checksums;
- SHA-256, inventario seguro y metadata `corral --version` antes de escribir;
- temporal en el directorio destino y reemplazo atómico;
- `--dry-run`, error autenticado claro y `--uninstall` conservador;
- plantilla Homebrew privada con URL/checksum por los cuatro targets.

## Gates locales

```sh
scripts/install/test-install.sh
scripts/release/test-homebrew-formula.sh
brew style /ruta/a/formula-generada/corral.rb
git diff --check
```

El test del instalador demostró que un checksum corrupto conserva byte a byte
el binario anterior. También verificó instalación offline ejecutable, dry-run
sin mutación, fallo sin token y uninstall sin tocar un state dir testigo. La
fórmula generada pasó sintaxis Ruby y `brew style`; no fue publicada.
Además se cargó como `djbu/corral-stage/corral` en un tap local temporal:
Homebrew reconoció versión, homepage, licencia y URL, y el tap se retiró al
terminar sin descargar ni instalar el archive ficticio.

La instalación online contra una release privada real y el consumo desde un
tap privado requieren una release candidate existente. Se ejecutarán en las
VMs limpias del paso 54; no se fabricó un tag ni se publicaron assets para
simular ese gate.
