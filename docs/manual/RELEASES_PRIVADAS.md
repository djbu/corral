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

## 5. Verificar e instalar manualmente

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

## 6. Rollback operativo

Si el binario nuevo falla pero no dañó el formato de datos:

1. detenga el daemon ordenadamente;
2. reinstale el archive de la versión anterior ya verificada;
3. arranque el daemon;
4. ejecute `corral --version`, `corral doctor` y `corral ls`;
5. registre el fallo y publique la corrección con una versión nueva.

Si hubo una migración de base de datos, restaure el backup previo. Una versión
vieja frente a un schema nuevo debe fallar claramente; no edite
`PRAGMA user_version` ni tablas a mano.

## 7. Revocar una release defectuosa

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
