# Konzept: Phasen-basierter, verifiziert-paralleler OCI-Builder für bootc-Systeme

**Name:** `nomira`
**Status:** Entwurf v1 · 2026-11-14 — historischer Stand. Recipe-Schema (`# syntax`,
`args`, `prepare`, String-Befehle) und Roadmap sind seitdem weiterentwickelt;
maßgeblich ist das README.

## 1. Ziel

Ein deklaratives Build-Werkzeug für bootc-Systemimages (primär Arch Linux), das
Build-Schritte in **Phasen** und **Gruppen** organisiert:

- Phasen laufen **seriell** in deklarierter Reihenfolge.
- Gruppen innerhalb einer Phase sind **deklariert unabhängig**: Sie werden als
  Fan-out auf demselben Basiszustand parallel gebaut, einzeln gecacht und am
  Phasenende zusammengeführt.
- Die Unabhängigkeit wird **im Build verifiziert** (Write-Set-Disjunktheit).
  Ein Konflikt bricht den Build ab, bevor gemergt oder exportiert wird.

Damit invalidiert die Änderung einer Gruppe nur deren eigenen Layer plus den
billigen Merge und nachfolgende Phasen – nicht die Geschwistergruppen. Das
überträgt Nix' Derivations-Granularität auf das Containerfile-Modell, ohne
dessen Flexibilität (beliebiges Shell, Arch-Pakete) aufzugeben.

## 2. Scope und Nicht-Ziele

**In Scope**

- Recipe-Format (YAML) → LLB-Graph → OCI-Image via BuildKit
- Konfliktvalidierung mit konfigurierbarer Policy
- Lieferung in podmans containers-storage und Übergabe an `bootc switch`
- Reproduzierbarkeits-Pinning über Arch Linux Archive (Snapshot-Datum)
- Post-Build-Gates (Hooks, bcvk-Boot-Test) im `switch`-Workflow

**Nicht-Ziele**

- Kein BlueBuild-Fork und keine Kompatibilität mit dessen Recipes/Modulen.
  Gemeinsame Basis ist nur bootc; die Build-Schicht ist ein eigenständiges
  Geschwisterprojekt.
- Keine Fedora-/Distro-spezifischen Module in v1. Das Recipe ist
  distro-agnostisch; Arch ist Referenz-Target.
- Kein eigener Paketmanager, kein Konfigurations-Merging à la Nix-Module.
- v1 ist kein `# syntax=`-Frontend (siehe Roadmap).

## 3. Build-Modell

### 3.1 Struktur

```
Pipeline  = geordnete Liste von Phasen          (Liste → Reihenfolge zählt)
Phase     = Menge unabhängiger Gruppen          (Mapping → Reihenfolge egal)
Gruppe    = serielle Liste von Befehlen         (Liste → Reihenfolge zählt)
```

Designprinzip: **Geordnetes ist Liste, Unabhängiges ist Mapping.** Die Syntax
spiegelt die Semantik; eine Phase mit genau einer Gruppe ist der Serienfall
(`base`, `finish`) ohne Sonderbehandlung.

### 3.2 Abbildung auf LLB

Für eine Phase mit Eingangszustand `S` und Gruppen `g₁…gₙ`:

```
S ──Exec/File(g₁)──→ R₁ ──DiffOp(S,R₁)──→ Δ₁   ┐
S ──Exec/File(g₂)──→ R₂ ──DiffOp(S,R₂)──→ Δ₂   ├─ parallel, einzeln gecacht
S ──Exec/File(gₙ)──→ Rₙ ──DiffOp(S,Rₙ)──→ Δₙ   ┘
S' = MergeOp(S, Δ₁, …, Δₙ)        → Eingang der nächsten Phase
```

- Jede Gruppe läuft **auf `S`**, nie auf dem Ergebnis einer Geschwistergruppe.
  Lese-Abhängigkeiten zwischen Gruppen sind damit konstruktiv ausgeschlossen
  (sie schlagen deterministisch fehl, statt still zu funktionieren).
- Befehle innerhalb einer Gruppe verketten sich seriell (ExecOp→ExecOp…),
  `copy` wird FileOp.
- Export: Jedes Δᵢ wird ein **eigener Image-Layer mit stabilem Digest**. Die
  Diffs sind kein Begleitartefakt – der Merge der Diffs *ist* das Image.
  Validierungsdaten (Write-Listen) landen nie im Export.

### 3.3 Cache-Semantik

- Änderung in Gruppe g → neu: Δ_g, MergeOp der Phase, alle Folgephasen.
- Unverändert: alle Geschwister-Δ (aus Cache, Digests stabil).
- MergeOp ist lazy/metadatenbasiert (overlay-artig), kein Kopieren.

## 4. Recipe-Schema v1

### 4.1 Beispiel

```yaml
version: 1

settings:
  name: meinsystem
  base-image: ghcr.io/…/arch-bootc@sha256:…   # per Digest gepinnt
  arch-snapshot: "2026/11/10"                 # Arch Linux Archive, s. §7
  validation: strict                          # Datei-Default

phases:
  - name: base
    groups:
      system:
        - run: pacman -Syu --needed --noconfirm kmscon networkmanager apparmor …

  - name: configs
    validation: allow-identical               # Phase überschreibt Default
    shared:                                   # Allowlist bewusst geteilter Pfade
      - /usr/lib/tmpfiles.d/common.conf
    groups:
      kmscon:
        - run: ./scripts/install-kmscon.sh
        - copy: [files/kmscon/, /etc/kmscon/]
      networkmanager:
        - copy: [configs/nm/, /etc/NetworkManager/]
      boot:
        - run: |
            python3 ./scripts/gen-boot-params.py \
              --out /usr/lib/bootc/kargs.d/

  - name: finish
    groups:
      enable:
        - run: systemctl enable kmscon NetworkManager apparmor
```

### 4.2 Felder

| Feld | Ebene | Bedeutung |
|---|---|---|
| `version` | Datei | Schema-Version, Pflicht |
| `settings.name` | Datei | Image-Name (`localhost/<name>`) |
| `settings.base-image` | Datei | Basis, Digest-Pinning empfohlen |
| `settings.arch-snapshot` | Datei | Archive-Datum; leer = ungepinnt (Warnung) |
| `settings.validation` | Datei | Default-Policy für alle Phasen |
| `phases[].name` | Phase | Pflicht, eindeutig; erscheint in Logs/Fehlern |
| `phases[].validation` | Phase | überschreibt Default |
| `phases[].shared` | Phase | Pfad-Allowlist, von der Konfliktprüfung ausgenommen |
| `phases[].groups.<name>` | Gruppe | Key = Gruppenname; Wert = Befehlsliste |
| `- run:` | Befehl | Shell (`/bin/sh -c`), Blockskalar für Mehrzeiler |
| `- copy: [src, dst]` | Befehl | aus dem Build-Kontext ins Image (FileOp) |

Bewusst **nicht** vorgesehen: Validation auf Gruppenebene. Disjunktheit ist
eine Eigenschaft von Gruppen-*Paaren* innerhalb einer Phase; eine Policy pro
Gruppe wäre bei einem Konflikt zwischen zwei Gruppen mehrdeutig.

Schema-Validierung (Struktur, unbekannte Keys, leere Phasen, doppelte
Gruppennamen) läuft vollständig **vor** dem ersten Build-Schritt.

## 5. Konfliktvalidierung

### 5.1 Write-Set-Erhebung

Pro Gruppe wird eine Write/Delete-Liste erhoben. Mechanik: ein internes ExecOp
bekommt `S` und `Rᵢ` nebeneinander gemountet und emittiert die Liste als
Datei, die das Frontend über die Gateway-API einliest. (Ein reines
`ReadDir`-Wandern des Diffs zeigt Deletions nicht zuverlässig; daher der
explizite Vergleichsschritt.) Die Listen existieren nur als Build-Referenzen
und werden nie exportiert.

### 5.2 Regeln

- **Gemeinsame Verzeichnisse sind nie ein Fehler.** Konflikt besteht nur auf
  Datei-Ebene (gleicher Pfad als Datei in ≥2 Gruppen).
- **Typ-Konflikt ist ein Fehler:** Gruppe A legt Datei `/etc/foo` an, Gruppe B
  das Verzeichnis `/etc/foo/`.
- **Whiteouts mit Präfix-Semantik:** Löscht eine Gruppe `/etc/foo/`
  (Verzeichnis-Whiteout), kollidiert das mit *jedem* Pfad unter `/etc/foo/`
  in jeder anderen Gruppe – nicht nur mit exakt gleichen Pfaden. Datei-Löschung
  kollidiert mit Schreiben desselben Pfads.
- Metadaten-Änderungen (chmod/chown ohne Inhaltsänderung) zählen als Schreiben
  des Pfads.

### 5.3 Policies

| Policy | Verhalten |
|---|---|
| `strict` | jeder Datei-Konflikt ist ein Fehler |
| `allow-identical` | gleicher Pfad erlaubt, wenn Inhalts-Hash identisch (Metadaten müssen ebenfalls übereinstimmen) |
| + `shared:` | Allowlist pro Phase, von der Prüfung ausgenommen (gilt für beide Policies) |

### 5.4 Fehlverhalten

Konflikt ⇒ **Build schlägt fehl**, vor MergeOp und Export. Fehlermeldung in
Recipe-Begriffen:

```
error: phase "configs": groups "kmscon" and "boot" both write
  /etc/systemd/system/getty@tty2.service.d/override.conf
  (policy: strict; add to phases[].shared to allow intentionally)
```

Erwarteter, gewollter Effekt: Zwei Gruppen, die beide `pacman -S` ausführen,
schreiben beide `/var/lib/pacman/local/…` und schlagen korrekt fehl –
Paketinstallationen sind über die gemeinsame Pacman-DB nie unabhängig und
gehören in eine serielle Phase (oder eine einzelne Gruppe).

## 6. Architektur

### 6.1 Sprache

**Go.** BuildKits `client/llb`-DSL und die Gateway-API sind Go-Bibliotheken
erster Klasse (inkl. MergeOp/DiffOp, Progress-UI, Solver-Client). Rust würde
Nachbau des LLB-Protobuf und Gateway-gRPC erfordern; vorhandene Crates sind
verwaist und stammen aus der Zeit vor MergeOp/DiffOp.

### 6.2 Paketstruktur

```
cmd/nomira/            CLI (cobra o. ä.)
internal/recipe/       YAML-Parsing, Schema-Validierung, Typen
internal/compile/      Recipe → LLB (Phasen/Gruppen → Exec/File/Diff/Merge)
internal/conflict/     Write-Set-Erhebung + Disjunktheitsprüfung + Policies
internal/engine/       buildkitd-Client: Solve, Progress, Export (oci-archive)
internal/deliver/      containers-storage-Import, bootc-switch-Aufruf
internal/gates/        Post-Build: Hook-Runner (validation.d/), bcvk-Gate
```

### 6.3 CLI

```
nomira build    [recipe.yml]   # buildkitd sicherstellen → kompilieren →
                               # solven (inkl. Konfliktprüfung) → oci-archive
nomira validate [image]        # Gates: validation.d/-Hooks, bcvk test-basic
nomira switch   [recipe.yml]   # build + validate + Import + bootc switch
nomira graph    [recipe.yml]   # DAG als Baum/Mermaid (zeigt Parallelität)
```

Grundsätze: Fehlermeldungen ausschließlich in Recipe-Begriffen (Phase, Gruppe,
Pfad – nie LLB-Vertex-IDs); Schema-Fehler vor dem ersten Build-Schritt;
BuildKits Progress-UI wird durchgereicht, sodass parallele Gruppen als
parallele Zeilen sichtbar sind. buildctl/skopeo/Daemon bleiben unsichtbar.

## 7. Laufzeitumgebung und Lieferung

- **Engine:** podman (einzige Laufzeit auf dem Host).
- **Builder:** rootless `buildkitd`, betrieben als podman-Container oder
  systemd-User-Service; `nomira build` startet/prüft ihn.
- **Lieferkette:**
  1. BuildKit-Export als `oci-archive`
  2. `skopeo copy oci-archive:… containers-storage:localhost/<name>`
  3. `bootc switch --transport containers-storage localhost/<name>`
- **Pinning:** `settings.arch-snapshot` setzt die mirrorlist im Build auf
  `archive.archlinux.org/repos/<datum>`. Das Datum ist die eine versionierte
  Stelle, die den Paketstand determiniert („flake.lock für Arme"):
  Update = Datum bumpen + rebuild, Rollback des Paketstands = Git-Revert.
  `base-image` wird per Digest gepinnt.
- **Generationen:** datierte oci-archives aufheben; optional lokale Registry
  (z. B. zot) für Digest-Historie beliebiger Tiefe.

## 8. Post-Build-Gates (außerhalb des Images)

Checks laufen gegen das fertige Image, nie als RUN-Layer:

```
validation.d/10-lint.sh      podman run --rm $IMG bootc container lint --fatal-warnings
validation.d/20-units.sh     podman run --rm $IMG systemd-analyze verify …
validation.d/30-apps.sh      podman run --rm $IMG sway --validate -c …
(fest eingebaut)             bcvk ephemeral test-basic $IMG   # Boot-Smoke-Test
```

`nomira switch` führt Build → Gates → Import → bootc switch aus; ein
fehlgeschlagenes Gate stoppt vor dem Import. Gates sind Shell-Hooks und damit
einzeilig erweiterbar.

## 9. Roadmap

- **v1 — CLI gegen buildkitd** (dieses Dokument): `client/llb` direkt, kein
  Frontend-Image nötig, `# syntax=` nicht im Spiel.
- **v2 — Gateway-Frontend:** dasselbe Go-Programm als Frontend-Image gebaut
  und in eine beliebige erreichbare OCI-Registry gepusht (ghcr.io, Docker Hub,
  localhost-Registry – es gibt keine privilegierte Liste und keinen
  Freigabeprozess). Danach baut jedes Vanilla-buildx Recipes mit
  `# syntax=ghcr.io/nomira-os/nomira:1-alpha`; testbar auch ohne Syntax-Zeile via
  `buildctl build --frontend gateway.v0 --opt source=<image>`.
- **Später/offen:** mehrstufige `allow`-Feinheiten (per-Pfad-Policy),
  `env:`/`workdir:` pro Gruppe, Remote-Cache.

## 10. Offene Punkte

- **buildkitd-Lifecycle:** autostart-Strategie (podman-Container vs.
  systemd-User-Unit), Versionsanforderung (MergeOp/DiffOp ⇒ BuildKit ≥ 0.10,
  real: aktuelle Version).
- **Arch-bootc-Basis** bleibt experimentell (bootcrew/mono); das Projekt ist
  davon unabhängig, erbt aber dessen Reifegrad für das Gesamtsystem.
- **Hardlinks/Xattrs im Diff:** Erhebungsschritt muss SELinux-/Security-Xattrs
  und Hardlink-Paare korrekt als Writes erfassen (Testfälle nötig).
- **`allow-identical`-Hash-Definition:** Inhalt + Modus + Owner + Xattrs, oder
  Inhalt allein? (Vorschlag: alles, strikt.)
