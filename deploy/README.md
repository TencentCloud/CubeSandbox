# deploy

Deployment entry directory for `cube-sandbox`.

Currently provides:

- `one-click/`: Offline release package solution targeting a single host machine.

Design goals:

- The build machine generates a complete release package from within the repository.
- The target machine extracts the package and installs everything with a single `install.sh` call.
- The runtime directory layout strictly follows the existing component conventions of `cube-sandbox`.
