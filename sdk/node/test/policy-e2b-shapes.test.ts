import { describe, expect, it } from "vitest";
import { convertE2BPerHostRules } from "../src/policy";

const perHost = (headers: unknown) => ({
  "api.example.com": [{ transform: { headers } }],
});

describe("convertE2BPerHostRules transform.headers validation", () => {
  it("rejects an array of header values", () => {
    expect(() => convertE2BPerHostRules(perHost(["sk-secret-1", "sk-secret-2"]) as never)).toThrow(
      /transform.headers must be an object/,
    );
  });

  it("rejects a string", () => {
    expect(() => convertE2BPerHostRules(perHost("sk-secret") as never)).toThrow(
      /transform.headers must be an object/,
    );
  });

  it("rejects a number", () => {
    expect(() => convertE2BPerHostRules(perHost(42) as never)).toThrow(
      /transform.headers must be an object/,
    );
  });

  it("rejects an array transform", () => {
    expect(() =>
      convertE2BPerHostRules({ "api.example.com": [["headers"]] } as never),
    ).toThrow(/transform/);
  });

  it("accepts a plain header map", () => {
    const rules = convertE2BPerHostRules(perHost({ Authorization: "sk-ok" }) as never);
    expect(rules).toHaveLength(1);
    expect(rules[0].action.inject).toEqual([{ header: "Authorization", secret: "sk-ok" }]);
  });

  it("accepts multiple headers in one transform", () => {
    const rules = convertE2BPerHostRules(
      perHost({ Authorization: "sk-a", "X-Api-Key": "sk-b" }) as never,
    );
    expect(rules[0].action.inject).toEqual([
      { header: "Authorization", secret: "sk-a" },
      { header: "X-Api-Key", secret: "sk-b" },
    ]);
  });
});
