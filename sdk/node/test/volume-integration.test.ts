// Copyright (c) 2026 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0

// End-to-end volume tests against a live CubeAPI. Skipped unless
// CUBE_RUN_INTEGRATION="1". Requires at least one volume plugin configured in
// CubeMaster/Cubelet; pin a specific backend with CUBE_VOLUME_TEST_DRIVER
// (empty default lets the backend pick its first configured plugin).
//
// Run with, e.g.:
//   CUBE_RUN_INTEGRATION=1 CUBE_API_URL=... CUBE_TEMPLATE_ID=... npm test

import { describe, expect, it } from "vitest";

import { Config } from "../src/config.js";
import { Sandbox, Volume, VolumeInUseError, VolumeMount, VolumeNotFoundError } from "../src/index.js";

const RUN_INTEGRATION = process.env.CUBE_RUN_INTEGRATION === "1";
const DRIVER = process.env.CUBE_VOLUME_TEST_DRIVER || undefined;

async function destroyWithRetry(volumeId: string, config: Config): Promise<void> {
  const deadline = Date.now() + 60_000;
  for (;;) {
    try {
      await Volume.destroy(volumeId, { config });
      return;
    } catch (err) {
      // Cubelet reports detach-side refcount changes asynchronously after a
      // sandbox is killed; retry briefly while the server says "in use".
      if (!(err instanceof VolumeInUseError) || Date.now() > deadline) {
        throw err;
      }
      await new Promise((resolve) => setTimeout(resolve, 2000));
    }
  }
}

describe.skipIf(!RUN_INTEGRATION)("volume integration (live CubeAPI)", () => {
  it(
    "covers the full volume lifecycle: CRUD, mounting, persistence, read-only, in-use protection",
    async () => {
      const config = new Config();
      const name = `node-sdk-e2e-${Date.now()}`;
      const mountPath = "/mnt/e2e-vol";
      const marker = `volume-data-${Date.now()}`;

      // -- Create / get / list --
      const volume = await Volume.create({ name, driver: DRIVER, config });
      try {
        expect(volume.volumeId).toBe(name);

        const info = await Volume.getInfo(volume.volumeId, { config });
        expect(info.volumeId).toBe(volume.volumeId);

        const volumes = await Volume.list({ config });
        const entry = volumes.find((v) => v.volumeId === volume.volumeId);
        expect(entry).toBeDefined();
        expect(entry?.token).toBe(""); // tokens are only surfaced on create/get-single

        await expect(
          Volume.getInfo("node-sdk-e2e-does-not-exist", { config }),
        ).rejects.toBeInstanceOf(VolumeNotFoundError);

        // -- Mount and write through the mount --
        const first = await Sandbox.create({
          config,
          timeout: 120,
          metadata: { sdk: "node", scenario: "integration-volume" },
          volumeMounts: { [mountPath]: volume },
        });
        try {
          const write = await first.commands.run(
            `printf %s ${marker} > ${mountPath}/persist.txt && cat ${mountPath}/persist.txt`,
          );
          expect(write.exitCode).toBe(0);
          expect(write.stdout).toBe(marker);

          const sandboxInfo = await first.getInfo();
          const mounts = (sandboxInfo.volumeMounts ?? []) as Array<Record<string, unknown>>;
          expect(mounts).toHaveLength(1);
          expect(mounts[0].name).toBe(volume.volumeId);
          expect(mounts[0].path).toBe(mountPath);

          // -- Deleting a mounted volume must be refused --
          await expect(Volume.destroy(volume.volumeId, { config })).rejects.toBeInstanceOf(
            VolumeInUseError,
          );
        } finally {
          await first.kill();
          first.close();
        }

        // -- Data must survive the sandbox lifecycle; read-only is enforced --
        const second = await Sandbox.create({
          config,
          timeout: 120,
          metadata: { sdk: "node", scenario: "integration-volume-readonly" },
          volumeMounts: { [mountPath]: new VolumeMount(volume, { readOnly: true }) },
        });
        try {
          const read = await second.commands.run(`cat ${mountPath}/persist.txt`);
          expect(read.exitCode).toBe(0);
          expect(read.stdout).toBe(marker);

          const roWrite = await second.commands.run(
            `touch ${mountPath}/should-fail.txt 2>&1`,
          );
          expect(roWrite.exitCode).not.toBe(0);
        } finally {
          await second.kill();
          second.close();
        }

        // -- After all sandboxes are gone the volume can be deleted --
        await destroyWithRetry(volume.volumeId, config);
        await expect(Volume.getInfo(volume.volumeId, { config })).rejects.toBeInstanceOf(
          VolumeNotFoundError,
        );
        // e2b-compatible idempotent destroy: already gone resolves false.
        await expect(Volume.destroy(volume.volumeId, { config })).resolves.toBe(false);
      } catch (err) {
        await destroyWithRetry(volume.volumeId, config).catch(() => {});
        throw err;
      }
    },
    300_000,
  );
});
