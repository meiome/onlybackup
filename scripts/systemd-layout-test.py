#!/usr/bin/env python3
"""Exercise the shipped mount rules in disposable systemd services.

Requires Linux and a running systemd manager. Non-root uses the user manager
and unprivileged user namespaces; root uses the system manager.
User/Group identity isolation is tested separately by make isolation-test.
"""
import os
from pathlib import Path
import subprocess
import tempfile

project = Path(__file__).resolve().parent.parent
with tempfile.TemporaryDirectory(prefix="onlybackup-systemd-") as temporary:
    fixture = Path(temporary)
    state = fixture / "state"
    for name in ("incoming", "archives/backups", "archives/quarantine", "maintenance", "runtime"):
        (state / name).mkdir(parents=True, mode=0o700, exist_ok=True)
    for name in ("metadata.db", "metadata.db-wal", "metadata.db-shm", "incoming/private"):
        (state / name).write_bytes(b"private writer state")
        (state / name).chmod(0o600)
    probe = fixture / "probe.test"
    subprocess.run([str(project / "scripts/go.sh"), "test", "-c", "-o", str(probe),
                    "./internal/maintenance"], cwd=project, check=True)
    # Use the actual shipped access directives, including catalog exclusions
    # and capability removal. CI runs as root: without the latter it can open
    # the mode-000 placeholders used by InaccessiblePaths, unlike our services.
    # PrivateUsers allows the unprivileged user manager to create mount namespaces;
    # BindReadOnlyPaths retains the fixture without making its parent writable.
    directives = ("ProtectSystem", "ProtectHome", "PrivateTmp", "PrivateDevices",
                  "NoNewPrivileges", "CapabilityBoundingSet", "AmbientCapabilities",
                  "ReadWritePaths", "InaccessiblePaths")
    for role in ("writer", "maintenance-split", "maintenance"):
        command = ["systemd-run"] + (["--user"] if os.geteuid() != 0 else [])
        command += ["--wait", "--pipe", "--collect",
                   "--property=PrivateUsers=yes", f"--property=BindReadOnlyPaths={fixture}"]
        unit_role = "writer" if role == "writer" else "maintenance"
        for line in (project / f"deploy/onlybackup-{unit_role}.service").read_text().splitlines():
            key, separator, value = line.partition("=")
            if separator and key in directives:
                value = value.replace("/var/lib/onlybackup", str(state))
                value = value.replace("/run/onlybackup", str(state / "runtime"))
                if role == "maintenance-split" and key == "ReadWritePaths":
                    value = value.replace(str(state / "archives"),
                                          f"{state / 'archives/backups'} {state / 'archives/quarantine'}")
                command.append(f"--property={key}={value}")
        command += [f"--setenv=ONLYBACKUP_SYSTEMD_STATE={state}",
                    f"--setenv=ONLYBACKUP_SYSTEMD_ROLE={unit_role}",
                    f"--setenv=ONLYBACKUP_SYSTEMD_SPLIT_MOUNTS={int(role == 'maintenance-split')}", str(probe),
                    "-test.run=^TestSystemdArchiveProbe$", "-test.v"]
        subprocess.run(command, check=True)
print("OK: real systemd mounts, writer hard-link publication, quarantine/recover/purge, catalog/incoming isolation.")
