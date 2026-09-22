import { describe, expect, it } from "vitest";
import { validateAllowOutDomainsRequireDenyAll } from "../src/policy";

const UNICODE_DOMAINS = [
  "example.com",
  "exаmple.com",
  "bücher.example",
  "例子.com",
  "café.example.com",
  "xn--bcher-kva.example",
  "*.café.example.com",
];

const NON_DOMAINS = ["10.0.0.1", "10.0.0.0/8", "::1", "999.1.2.3", "", "has space"];

describe("allow_out domain guard", () => {
  for (const d of UNICODE_DOMAINS) {
    it(`fires for ${d} when public egress is on`, () => {
      expect(() => validateAllowOutDomainsRequireDenyAll([d], [], false)).toThrow();
    });

    it(`does not fire for ${d} when deny-all is present`, () => {
      expect(() => validateAllowOutDomainsRequireDenyAll([d], ["0.0.0.0/0"], false)).not.toThrow();
      expect(() => validateAllowOutDomainsRequireDenyAll([d], [], true)).not.toThrow();
    });
  }

  for (const t of NON_DOMAINS) {
    it(`does not treat ${JSON.stringify(t)} as a domain`, () => {
      expect(() => validateAllowOutDomainsRequireDenyAll([t], [], false)).not.toThrow();
    });
  }
});
