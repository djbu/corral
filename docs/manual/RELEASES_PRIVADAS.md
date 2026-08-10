# Operar releases privadas de corral

Este runbook explica cómo construir, publicar, verificar y revocar los
artefactos privados introducidos en M7B. Está dirigido a quien mantiene el
repositorio `djbu/corral`; instalar una release ya publicada requiere sólo las
secciones 4 y 5.

corral sigue en pre-alpha. Los binarios de macOS todavía no están firmados ni
notarizados y una release M7B no equivale al cierre de `v0.7.0`.

## 1. Inventario esperado

Cada release contiene cuatro archives:

```text
corral_<version>_darwin_amd64.tar.gz
corral_<version>_darwin_arm64.tar.gz
corral_<version>_linux_amd64.tar.gz
corral_<version>_linux_arm64.tar.gz
```

También contiene un archivo de checksums SHA-256, un SBOM SPDX JSON por
archive y `metadata.json`. `artifacts.json` se conserva sólo durante el build
porque incluye rutas internas del runner. Cada archive contiene exactamente
`corral`, `README.md` y `LICENSE`.

## 2. Reproducir localmente

Instale GoReleaser `v2.17.1` y Syft `v1.50.0`, y use el Go fijado en
`.go-version`. Desde un tag anotado:

```sh
scripts/release/verify-tag.sh v0.7.0-rc.1
scripts/release/test-reproducible.sh v0.7.0-rc.1
```

El segundo comando clona dos veces el mismo commit, construye los cuatro
targets, valida checksums/SBOM/versiones y compara los SHA-256 de los archives.
No publica nada.

## 3. Publicar

Antes de crear un tag, el commit debe tener verdes los checks
`test (macos-latest)` y `test (ubuntu-latest)`.

```sh
git status --short
git tag -a v0.7.0-rc.1 -m "corral v0.7.0-rc.1"
git push origin v0.7.0-rc.1
```

El workflow `release` rechaza tags ligeros, versiones no SemVer, checkout
sucio, identidad de módulo incorrecta, checks faltantes y versiones ya
publicadas. GoReleaser crea primero un draft privado. El workflow abre los
archives y sólo convierte el draft en release visible después de verificarlo.
Los sufijos `-rc.N`, `-beta.N` y similares quedan marcados como prerelease.

Nunca mueva ni reutilice un tag publicado.

## 4. Descargar desde el repositorio privado

Autentique GitHub CLI con una identidad que tenga acceso al repositorio:

```sh
gh auth status
mkdir corral-release
gh release download v0.7.0-rc.1 \
  --repo djbu/corral \
  --dir corral-release
```

También puede usar un `GH_TOKEN` de alcance mínimo. No guarde el token en el
repositorio ni lo pase en una URL.

## 5. Instalar automáticamente

Desde un checkout privado que contenga M7D, use una versión explícita:

```sh
export GH_TOKEN='token-read-only'
scripts/install.sh --version 0.7.0-rc.1
```

`CORRAL_GITHUB_TOKEN` y `GITHUB_TOKEN` son alternativas. El instalador descarga
con `gh`, verifica SHA-256, inventario y metadata, y reemplaza
`$HOME/.local/bin/corral` atómicamente. `--prefix`, `--dry-run`, `--archive` y
`--checksums` cubren prefijos alternativos, inspección y traslado offline. Para
retirar sólo el binario, sin borrar estado ni servicio, use
`scripts/install.sh --uninstall`.

## 6. Verificar e instalar manualmente

Desde el checkout correspondiente al tag:

```sh
commit="$(git rev-parse 'v0.7.0-rc.1^{}')"
scripts/release/verify-artifacts.sh \
  corral-release 0.7.0-rc.1 "$commit"
```

Después extraiga únicamente el archive de su sistema:

```sh
tar -xzf corral-release/corral_0.7.0-rc.1_darwin_arm64.tar.gz
./corral --version
install -m 0755 corral "$HOME/.local/bin/corral"
```

En macOS, al no existir todavía notarización, Gatekeeper puede exigir una
confirmación manual. No quite atributos de cuarentena a un archivo que no haya
pasado primero checksum y verificación de versión.

## 7. Homebrew privado preparado

No existe un tap público. Para una release privada, capture también los
endpoints API de sus assets; las URLs web `/releases/download/...` responden
404 a curl para un repositorio privado:

```sh
gh release view v0.7.0 --repo djbu/corral --json assets \
  > dist/corral_0.7.0_assets.json
scripts/release/render-homebrew-formula.sh \
  0.7.0 dist/corral_0.7.0_checksums.txt \
  /ruta/al/tap-privado/Formula/corral.rb \
  dist/corral_0.7.0_assets.json
```

El consumidor autentica la descarga privada mediante
`HOMEBREW_GITHUB_API_TOKEN`. El gate limpio del paso 54 instaló y probó la
fórmula real desde un tap efímero; la plantilla jamás se publica con hashes
ficticios.

## 8. Rollback operativo

Si el binario nuevo falla pero no dañó el formato de datos:

1. detenga el daemon ordenadamente;
2. reinstale el archive de la versión anterior ya verificada;
3. arranque el daemon;
4. ejecute `corral --version`, `corral doctor` y `corral ls`;
5. registre el fallo y publique la corrección con una versión nueva.

Si hubo una migración de base de datos, restaure el backup previo. Una versión
vieja frente a un schema nuevo debe fallar claramente; no edite
`PRAGMA user_version` ni tablas a mano.

## 9. Revocar una release defectuosa

No reemplace bytes bajo el mismo nombre:

```sh
gh release edit v0.7.0-rc.1 --repo djbu/corral --prerelease
```

Añada a las notas la causa, impacto y versión de reemplazo. Si la release nunca
fue consumida y debe retirarse por completo, elimine la release pero conserve
el tag y el registro del incidente. Publique el arreglo como `rc.2` o una
versión posterior.

La única excepción es el tag temporal reservado para el gate M7B: después de
descargar y verificar la preview de prueba se eliminan tanto su release como su
tag remoto, y se registra el SHA validado en el PR.
