# M7D — Instalación privada verificable

**Alcance:** paso 52 de M7. **Publicación pública:** fuera de alcance mientras
`djbu/corral` sea privado.

## 1. Contrato

`scripts/install.sh` instala una versión explícita en un prefijo de usuario. El
default es `$CORRAL_INSTALL_PREFIX` o `$HOME/.local`, y el único archivo que
crea como producto es `<prefix>/bin/corral`.

Hay dos fuentes equivalentes:

- online: `gh release download` contra `djbu/corral`, autenticado únicamente
  mediante `CORRAL_GITHUB_TOKEN`, `GH_TOKEN` o `GITHUB_TOKEN`;
- offline: `--archive` y `--checksums`, transportados juntos por un canal que
  el operador controle.

La versión, el OS, la arquitectura y el nombre del asset deben coincidir. El
instalador verifica SHA-256, inventario del tar y `corral --version` antes de
crear un temporal en el directorio destino y reemplazarlo con `rename(2)` vía
`mv`. Un fallo deja intacto el binario anterior.

## 2. Límites de seguridad

- se rechazan root, prefijos relativos y `/`;
- no se aceptan tokens por argumento, URL ni archivo;
- un archive offline necesita el archivo de checksums de la misma release;
- sólo se extraen archives con `LICENSE`, `README.md` y un `corral` regular;
- `--dry-run` online no usa red; offline verifica completamente sin instalar;
- `--uninstall` elimina sólo `<prefix>/bin/corral` y conserva datos, logs,
  configuración y definiciones del servicio.

El instalador no gestiona `corral service`: actualizar un binario en la misma
ruta es compatible con M7C; cambiar de prefijo exige reinstalar explícitamente
la definición del servicio.

## 3. Homebrew privado preparado

`packaging/homebrew/Formula/corral.rb.in` es una plantilla, no un tap publicado.
`scripts/release/render-homebrew-formula.sh` la materializa sólo después de que
existan los cuatro checksums de una release. La fórmula exige
`HOMEBREW_GITHUB_API_TOKEN`, usa URLs versionadas y fija SHA-256 por OS/CPU.

Esto evita subir una fórmula con hashes ficticios. El gate local valida su
sintaxis Ruby y sus cuatro targets; el smoke real con Homebrew y una release
privada se ejecuta en el paso 54, junto con las VMs limpias.

## 4. Gates

`scripts/install/test-install.sh` cubre instalación offline, dry-run,
preservación del binario ante checksum inválido, error sin credenciales y
desinstalación conservadora. `scripts/release/test-homebrew-formula.sh` cubre la
materialización determinista de la fórmula privada.
