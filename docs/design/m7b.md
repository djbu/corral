# M7B — Builds reproducibles y publicación privada

**Estado:** implementado y validado en `djbu/corral#2`.
**Alcance:** pasos 48–49 de `docs/roadmap/POST_M6.md`.  
**Release final de M7:** fuera de alcance; M7B no crea `v0.7.0`.

## 1. Objetivo

Convertir un tag Git anotado en un inventario privado y verificable de
artefactos para:

- `darwin/amd64`;
- `darwin/arm64`;
- `linux/amd64`;
- `linux/arm64`.

Cada archivo debe contener un único binario estático `corral`, `README.md` y
`LICENSE`. La publicación incluye checksums SHA-256, un SBOM SPDX JSON por
archivo y metadatos suficientes para auditar el commit de origen.

## 2. No objetivos

- firmar o notarizar binarios de macOS;
- publicar Homebrew, paquetes del sistema o imágenes de contenedor;
- hacer público el repositorio o sus releases;
- crear el tag estable `v0.7.0` antes de cerrar los pasos 50–54;
- aceptar tags ligeros o reconstruir artefactos ya publicados.

La ausencia de firma/notarización mantiene la release como preview privada y
debe indicarse en las notas y manuales.

## 3. Contrato de versión

Los tags aceptados cumplen SemVer y empiezan por `v`, por ejemplo `v0.7.0` o
`v0.7.0-rc.1`. Deben ser objetos tag anotados, no referencias ligeras.

GoReleaser elimina el prefijo `v` e inyecta en el binario:

```text
Version = 0.7.0-rc.1
Commit  = <SHA-1 completo del commit apuntado por el tag>
```

`corral --version` debe imprimir exactamente:

```text
corral 0.7.0-rc.1 (<SHA-1 completo>, api 1)
```

El API versionado sigue siendo una constante del código. El verificador de
artefactos rechaza cualquier valor distinto de `1`.

## 4. Reproducibilidad

El build usa una versión explícita de GoReleaser y la versión de Go fijada por
`go.mod`. Para eliminar entradas dependientes de máquina o reloj:

- `CGO_ENABLED=0`;
- `-trimpath` y `-buildvcs=false`;
- `-mod=readonly`;
- `Version` y `Commit` son los únicos valores inyectados;
- el `mtime` del binario, archivos del archive y metadata deriva del timestamp
  del commit;
- nombres, owner, group, modos y formato de archivos son deterministas;
- no se usa `{{ .Date }}`, `{{ .Now }}`, `{{ .Timestamp }}` ni hora de pared;
- `dist/` nunca se versiona.

El gate compara dos builds limpios del mismo tag/configuración. Deben producir
el mismo conjunto de nombres y los mismos SHA-256 para los cuatro archives.
Los SBOM se validan estructuralmente, pero no forman parte del gate de igualdad
byte a byte: su herramienta puede ordenar metadata de forma distinta sin
cambiar los artefactos que describe.

## 5. Inventario

Para una versión `0.7.0-rc.1` se esperan:

```text
corral_0.7.0-rc.1_darwin_amd64.tar.gz
corral_0.7.0-rc.1_darwin_arm64.tar.gz
corral_0.7.0-rc.1_linux_amd64.tar.gz
corral_0.7.0-rc.1_linux_arm64.tar.gz
corral_0.7.0-rc.1_checksums.txt
<archive>.sbom.json (uno por archive)
```

`dist/artifacts.json` y `dist/metadata.json` son evidencia local del build.
`metadata.json` se publica con la release; `artifacts.json` queda en el runner
porque contiene rutas locales y no forma parte del contrato descargable.

## 6. Gates locales

`scripts/release/verify-artifacts.sh` recibe versión, commit y directorio. Debe:

1. exigir exactamente los cuatro archives esperados;
2. rechazar archivos inesperados en el inventario publicable;
3. comprobar cada entrada del archivo de checksums;
4. extraer cada archive en un directorio temporal;
5. verificar que contiene `corral`, `README.md` y `LICENSE`, sin traversal;
6. ejecutar directamente el binario nativo cuando coincida con host/arch;
7. para targets no nativos, inspeccionar formato/arquitectura y extraer el
   string de versión sin intentar ejecutarlo;
8. verificar `Version`, SHA completo y `api 1`;
9. parsear cada SBOM como JSON y comprobar que referencia su archive.

`scripts/release/test-reproducible.sh` realiza dos builds con directorios
`dist` separados y compara nombres y SHA-256 de archives.

## 7. Publicación y permisos

`.github/workflows/release.yml` sólo responde a `push.tags: ["v*"]` y aplica:

1. checkout completo del tag, sin persistir credenciales;
2. validación local del tag antes de instalar herramientas;
3. consulta de checks del commit mediante la API de GitHub;
4. exigencia de éxito para `test (macos-latest)` y
   `test (ubuntu-latest)`;
5. Go, GoReleaser y Syft en versiones explícitas;
6. build y verificación antes de conceder el paso de publicación;
7. creación de GitHub Release con `contents: write` y ningún otro permiso de
   escritura.

El job usa `concurrency` por tag sin `cancel-in-progress`: una segunda entrega
del mismo nombre no debe sustituir silenciosamente a la primera. GoReleaser
marca automáticamente como prerelease cualquier SemVer con sufijo prerelease.

## 8. Amenazas y fallos cerrados

| Riesgo | Control |
|---|---|
| Tag ligero o movido | Se exige objeto `tag` anotado y coincidencia tag→HEAD. |
| Tag sobre código no validado | Se consultan los dos checks obligatorios del SHA. |
| Árbol o módulo modificado durante build | Checkout limpio, `-mod=readonly` y validación de dirty state. |
| Artefacto de otra versión/commit | Verificador independiente abre los cuatro archives. |
| Path traversal en archive | Se rechazan rutas absolutas, `..` y entradas fuera del inventario. |
| Dependencia comprometida de Actions | Actions y herramientas se fijan a versiones revisadas; permisos mínimos. |
| Token filtrado | Sólo `GITHUB_TOKEN`; no se imprime ni se guarda en artefactos. |
| Sobrescritura de una release | Un tag/version ya existente falla; rollback no mueve tags. |
| Release parcial | GoReleaser construye/verifica antes de publicar; un fallo posterior se revoca según runbook. |

## 9. Rollback y revocación

Una release defectuosa no se reemplaza:

1. marcarla como prerelease y documentar el motivo;
2. retirar sus artefactos o eliminar la release sólo si todavía no fue
   consumida, conservando registro del incidente;
3. no mover ni reutilizar el tag;
4. corregir en un commit nuevo y publicar una versión SemVer nueva;
5. para una preview temporal de validación, eliminar release y tag al terminar,
   porque su nombre está reservado explícitamente para el test M7B.

## 10. Gate de cierre M7B

- configuración validada por GoReleaser;
- suite normal verde;
- dos builds limpios producen archives idénticos;
- los cuatro binarios anuncian versión, commit y API correctos;
- workflow rechaza tag ligero, versión divergente y checks incompletos;
- prerelease temporal anotada publicada y descargada desde el repositorio
  privado;
- checksums y SBOM verificados después de descargar;
- release y tag temporal retirados;
- manuales y roadmap actualizados;
- cambios entregados mediante PR protegido.
