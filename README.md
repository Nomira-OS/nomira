# nomira

> **Alpha.** Das Recipe-Schema kann sich noch ohne Ankündigung ändern.
> Recipes verweisen deshalb auf `ghcr.io/nomira-os/nomira:1-alpha`; mit
> dem stabilen Schema wird daraus `:1`.

Ein BuildKit-Gateway-Frontend für phasen-basierte, **verifiziert-parallele**
OCI-Builds von bootc-Systemimages (primär Arch Linux). Siehe `docs/concept.md`
(Konzept v1; beim Recipe-Schema gilt dieses README) für Motivation und Modell.

- **Phasen** laufen seriell in deklarierter Reihenfolge.
- **Gruppen** innerhalb einer Phase sind deklariert unabhängig: Fan-out auf
  demselben Basiszustand, parallel gebaut, einzeln gecacht (DiffOp), am
  Phasenende zusammengeführt (MergeOp).
- Die Unabhängigkeit wird **im Build verifiziert** (Write-Set-Disjunktheit
  mit Whiteout-Präfix-Semantik). Ein Konflikt bricht den Build ab, bevor
  gemergt oder exportiert wird.

```
S ──Exec(g₁)──→ R₁ ──Diff──→ Δ₁  ┐
S ──Exec(g₂)──→ R₂ ──Diff──→ Δ₂  ├─ parallel, einzeln gecacht, validiert
S ──Exec(gₙ)──→ Rₙ ──Diff──→ Δₙ  ┘
S' = Merge(S, Δ₁ … Δₙ)
```

## Recipe

```yaml
# syntax=ghcr.io/nomira-os/nomira:1-alpha
settings:
  name: meinsystem
  description: Mein System                  # wird zum Label org.opencontainers.image.description
  base: ghcr.io/nomira-os/nomira-image:2026.11.13
  validation: strict                        # Datei-Default: strict | allow-identical
  labels:                                   # Labels am fertigen Image
    org.example.owner: ich
  env:                                      # Umgebung für alle run-Befehle (nur beim Bauen)
    LANG: C.UTF-8
  args:
    VERSION: "1.0"                          # ${VERSION} in Befehlen; --build-arg überschreibt
  platforms: [linux/amd64]                  # Angabe für CLI und CI, siehe unten
  tags: ["${VERSION}", stable]              # Angabe für CLI und CI, siehe unten

prepare:                                    # optionale benannte Stages (Multistage)
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands:
      - run --no-cache git clone https://aur.archlinux.org/example.git /src

phases:                                     # Liste: Reihenfolge zählt
  - name: packages
    groups:                                 # Mapping: Gruppen unabhängig, Key = Name
      system:                               # einzelne Gruppe = serieller Fall, keine Validierung
        - run --mount=type=cache,target=/usr/lib/sysimage/cache/pacman/pkg pacman -S --needed --noconfirm kmscon
  - name: configs
    validation: allow-identical             # Phase überschreibt Default
    shared: [/usr/lib/tmpfiles.d/]          # Allowlist (Suffix "/" = Präfix)
    env: {MAKEFLAGS: -j4}                   # Umgebung für diese Phase
    groups:
      kmscon:
        - env THEME=dark                    # gilt für die folgenden Befehle der Gruppe
        - copy --chmod=0644 files/kmscon/ /etc/kmscon/
      aur:
        - copy --from=aurbuild /out/ /usr/lib/aur/
```

Befehle sind Strings mit Verb am Anfang (Mehrzeiler als YAML-Blockskalar):

| Befehl | Flags |
|---|---|
| `run <shell>` | `--no-cache`, `--mount=type=cache,target=/pfad[,id=…][,sharing=shared\|private\|locked]` |
| `copy <src>… <dst>` | `--from=<stage>`, `--chmod=0644`, `--chown=UID:GID` |
| `env NAME=WERT` | keine; alles nach dem ersten `=` ist der Wert |

**Args:** `${NAME}` wird nur für in `settings.args` deklarierte Args ersetzt;
`$repo` u. ä. bleiben unangetastet. Ein geänderter Arg invalidiert genau die
Befehle, die ihn verwenden.

**Umgebung:** `settings.env` gilt für alle `run`-Befehle, `env` an einer
Phase für deren Gruppen, der Befehl `env` für den Rest seiner Gruppe. Das
Speziellere gewinnt. Die Variablen gelten nur beim Bauen und landen nicht
in der Image-Konfiguration.

**`run --no-cache`:** Der Schritt läuft immer, auch wenn BuildKit ihn im
Cache hat. Danach läuft alles neu, was auf ihm aufbaut: der Rest seiner
Gruppe und alle folgenden Phasen. Solche Schritte gehören deshalb in eine
eigene Gruppe möglichst weit hinten oder in eine `prepare`-Stage.

**`platforms` und `tags`:** Das Frontend prüft beide Angaben und gibt sie
über `inspect` aus, wertet sie aber nicht aus. Für welche Architektur
gebaut und unter welchen Tags veröffentlicht wird, entscheidet der
Aufrufer (CLI oder CI).

## Aufteilen auf mehrere Dateien

Ein Recipe hat eine Einstiegsdatei; alles Weitere lässt sich mit `include`
auslagern. Pfade gelten relativ zur einbindenden Datei und müssen im
Recipe-Verzeichnis bleiben. Eingebundene Dateien dürfen selbst wieder
einbinden; Zyklen sind ein Fehler.

```yaml
prepare:
  aurbuild:
    from: docker.io/library/archlinux:base-devel
    commands: {include: stages/aur.yml}     # Datei mit einer Befehlsliste

phases:
  - include: phases/base.yml                # Datei mit einer Phase oder einer Liste von Phasen
  - name: configs
    groups:
      kmscon: {include: groups/kmscon.yml}  # Datei mit einer Befehlsliste
      networkmanager:
        - copy configs/nm/ /etc/NetworkManager/
```

Überall, wo eine Befehlsliste steht, geht stattdessen `include: <datei>`;
in der Phasenliste bindet `- include: <datei>` ganze Phasen ein. `settings`
steht nur in der Einstiegsdatei. Quellpfade von `copy` beziehen sich
weiterhin auf den Build-Kontext, nicht auf die eingebundene Datei.

## Bauen eines Recipes

Das Frontend-Image liegt unter `ghcr.io/nomira-os/nomira`, für
`linux/amd64` und `linux/arm64`, signiert mit cosign:

- **Releases** entstehen von Hand über den Workflow „release“ (`beta`
  oder `final`) und tragen die Versionsnummer als Tag. Jedes Release
  bewegt außerdem `1-alpha`, auf das Recipes verweisen.
- **Jeder Commit auf `main`** wird als unveränderlicher Entwicklungsstand
  `sha-<commit>` veröffentlicht.

Version und Changelog ergeben sich aus den Conventional Commits, siehe
[Nomira-OS/ci](https://github.com/Nomira-OS/ci). Lokal bauen geht
weiterhin:

```sh
podman build -f Dockerfile.frontend -t ghcr.io/nomira-os/nomira:1-alpha .
```

Dann baut jedes Vanilla-BuildKit das Recipe — die `# syntax=`-Zeile wählt
das Frontend selbst aus:

```sh
buildctl build \
  --frontend dockerfile.v0 \
  --local context=. --local dockerfile=. \
  --opt filename=recipe.yml \
  --opt build-arg:SNAPSHOT=2026/12/01 \
  --output type=oci,dest=img.tar
```

Oder explizit, ohne `# syntax`-Zeile (praktisch zum Testen lokaler
Frontend-Builds):

```sh
buildctl build \
  --frontend gateway.v0 --opt source=ghcr.io/nomira-os/nomira:1-alpha \
  --local context=. --local dockerfile=. \
  --opt filename=recipe.yml \
  --output type=oci,dest=img.tar
```

Lieferung an podman/bootc:

```sh
skopeo copy oci-archive:img.tar containers-storage:localhost/meinsystem
bootc switch --transport containers-storage localhost/meinsystem
```

## Zusatzmodi (für dünne CLIs)

Dasselbe Binary dient als Parser-Orakel, damit eine CLI in beliebiger
Sprache (z. B. Rust) Schema-Validierung und Graph nicht nachbauen muss:

```sh
nomira-frontend lint  recipe.yml     # Parse + Schema; Exit 1 mit Fehlern
nomira-frontend graph recipe.yml     # Pipeline als Mermaid-Flowchart
nomira-frontend inspect recipe.yml   # Name, Basis, Phasen und Gruppen als JSON
podman run --rm -i ghcr.io/nomira-os/nomira:1-alpha lint - < recipe.yml
```

## Entwicklung

Mit normalem Netz genügt:

```sh
go mod tidy && make test build
```

`hack/gen_offline_deps.py` existiert für Umgebungen ohne Go-Modul-Proxy:
Es erzeugt eine `go.offline.mod`, die alle Abhängigkeiten per `replace` aus
einem lokalen `moby/buildkit`-Checkout (dessen `vendor/`-Baum) bedient;
gebaut wird dann mit `GOFLAGS="-mod=mod -modfile=go.offline.mod"`. Das
normale `go.mod` bleibt davon unberührt.

Tests: `go test ./...` (Parser, Konfliktprüfung) plus
`hack/test_lister.sh`, der `internal/compile/lister.sh` gegen synthetische
Bäume prüft (Add/Change/Metadaten/Symlink/Datei- und Verzeichnis-Löschung/
Leerzeichen-Pfade).

## Status / bekannte Grenzen (v1-Gerüst)

- Write-Set-Erhebung hasht den gesamten Upper-Baum einer Gruppe; Phasen mit
  nur einer Gruppe (z. B. die pacman-Basisphase) überspringen Diff, Merge
  und Validierung vollständig.
- `copy` unterstützt keine Pfade mit Leerzeichen in der Befehlszeile (die
  Validierung selbst schon); `--chown` nur numerisch.
- Xattrs/Hardlinks werden vom Lister noch nicht als eigene Writes erfasst
  (concept.md §10).
- Der Lister braucht ein Toolbox-Image mit busybox-Werkzeugen
  (`settings.toolbox`, Default `docker.io/library/busybox:1.36.1`).
- Plattform folgt dem Build-Host; Multi-Arch ist offen.
- Gegen BuildKit v0.35.0 gebaut; MergeOp/DiffOp brauchen ≥ 0.10.

## Lizenz

EUPL-1.2, siehe `LICENSES/`. Die Angaben je Datei stehen in `REUSE.toml`.
