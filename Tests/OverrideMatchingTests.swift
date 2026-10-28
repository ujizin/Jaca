import XCTest
@testable import Jaca

/// The frozen matcher semantics. This table *is* the specification — the UI's hint text
/// ("`*` one segment · `**` any depth · query ignored unless you write `?`") promises exactly
/// these behaviours, so a change here is a change to the product.
final class OverrideMatchingTests: XCTestCase {

    // MARK: - Helpers

    private func rule(_ pattern: String,
                      kind: OverrideMatcher.Kind = .glob,
                      methods: Set<String> = []) -> CompiledRule {
        let r = OverrideRule(matcher: OverrideMatcher(pattern: pattern, kind: kind, methods: methods))
        let set = OverrideCompiler.compile([r], masterEnabled: true)
        guard let compiled = set.rules.first else {
            XCTFail("pattern failed to compile: \(pattern)")
            return CompiledRule(rule: r, program: nil, regex: nil)
        }
        return compiled
    }

    private func matches(_ pattern: String, _ url: String,
                         method: String = "GET",
                         kind: OverrideMatcher.Kind = .glob,
                         methods: Set<String> = []) -> Bool {
        guard let facts = OverrideMatching.facts(url: url) else { return false }
        return OverrideMatching.matches(rule(pattern, kind: kind, methods: methods), facts, method: method)
    }

    // MARK: - URL parsing

    func test_facts_parsesSchemeHostPathAndQuery() {
        let f = OverrideMatching.facts(url: "https://API.Example.com/v1/Users?a=1&a=2&b=x")
        XCTAssertEqual(f?.scheme, "https")
        XCTAssertEqual(f?.host, "api.example.com")       // host lowercased
        XCTAssertEqual(f?.path, "/v1/Users")             // path case preserved
        XCTAssertEqual(f?.query["a"], ["1", "2"])
        XCTAssertEqual(f?.query["b"], ["x"])
        XCTAssertNil(f?.port)
    }

    func test_facts_pathDefaultsToSlash() {
        XCTAssertEqual(OverrideMatching.facts(url: "https://example.com")?.path, "/")
    }

    /// Companion flow-metadata rows are `"host:port"`, not URLs. They must never match a rule —
    /// there is no HTTP exchange there to override.
    func test_facts_rejectsCompanionMetadataForm() {
        XCTAssertNil(OverrideMatching.facts(url: "api.example.com:443"))
        XCTAssertNil(OverrideMatching.facts(url: ""))
        XCTAssertNil(OverrideMatching.facts(url: "not a url"))
    }

    // MARK: - Anchoring

    func test_patternMustMatchTheWholeURL() {
        XCTAssertFalse(matches("https://a.com/v1", "https://a.com/v1/users"))
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1"))
        XCTAssertTrue(matches("https://a.com/v1/**", "https://a.com/v1/users"))
    }

    // MARK: - `*` vs `**`

    func test_singleStarMatchesOneSegmentOnly() {
        XCTAssertTrue(matches("https://a.com/v1/*/state", "https://a.com/v1/abc/state"))
        XCTAssertFalse(matches("https://a.com/v1/*/state", "https://a.com/v1/abc/def/state"))
    }

    func test_globstarCrossesSlashes() {
        XCTAssertTrue(matches("https://a.com/**/state", "https://a.com/v1/abc/def/state"))
        XCTAssertTrue(matches("https://a.com/**", "https://a.com/a/b/c/d"))
    }

    /// A `*` that runs out of room must hand back to the `**` before it.
    ///
    /// The matcher kept a single backtrack slot, so a later `*` overwrote the globstar's — and
    /// since `*` may not cross `/`, it could never recover. Both of these have a valid match that
    /// the matcher used to miss, silently: the rule never fired and the editor's preview said
    /// "nothing matches", with nothing anywhere to explain why.
    func test_starFallsBackToAnEarlierGlobstar() {
        // `**` must give up the first `/api/` and take `x.com/api/v2` instead.
        XCTAssertTrue(matches("**/api/*", "https://x.com/api/v2/api/thing"))
        // `**` → `z/v1/q`, then the literal `/v1/`, then `*` → `ab`, then `x`.
        XCTAssertTrue(matches("https://a.com/**/v1/*x", "https://a.com/z/v1/q/v1/abx"))
    }

    /// The fall-back must not turn `*` into `**` — a genuine non-match stays a non-match.
    func test_backtrackingStillRefusesToLetAStarCrossASlash() {
        XCTAssertFalse(matches("https://a.com/*", "https://a.com/one/two"))
        XCTAssertFalse(matches("https://a.com/**/v1/*x", "https://a.com/z/v1/q/ab/x"))
    }

    func test_globstarMatchesEmpty() {
        XCTAssertTrue(matches("https://a.com/v1**", "https://a.com/v1"))
    }

    // MARK: - Scheme

    func test_omittedSchemeMatchesEither() {
        XCTAssertTrue(matches("a.com/v1", "https://a.com/v1"))
        XCTAssertTrue(matches("a.com/v1", "http://a.com/v1"))
    }

    func test_explicitSchemeIsRequired() {
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1"))
        XCTAssertFalse(matches("https://a.com/v1", "http://a.com/v1"))
    }

    // MARK: - Host / port / case

    func test_hostIsCaseInsensitive_pathIsCaseSensitive() {
        XCTAssertTrue(matches("https://API.example.com/v1", "https://api.EXAMPLE.com/v1"))
        XCTAssertFalse(matches("https://a.com/Users", "https://a.com/users"))
    }

    func test_portComparedOnlyWhenPatternNamesOne() {
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com:8443/v1"))
        XCTAssertTrue(matches("https://a.com:8443/v1", "https://a.com:8443/v1"))
        XCTAssertFalse(matches("https://a.com:9999/v1", "https://a.com:8443/v1"))
    }

    // MARK: - Query

    func test_queryIgnoredUnlessPatternHasQuestionMark() {
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1?locale=en&x=1"))
    }

    func test_queryRequirementIsASubsetMatch() {
        XCTAssertTrue(matches("https://a.com/v1?locale=en", "https://a.com/v1?locale=en&extra=1"))
        XCTAssertTrue(matches("https://a.com/v1?locale=en", "https://a.com/v1?extra=1&locale=en"))
        XCTAssertFalse(matches("https://a.com/v1?locale=en", "https://a.com/v1?locale=fr"))
        XCTAssertFalse(matches("https://a.com/v1?locale=en", "https://a.com/v1"))
    }

    func test_queryValueWildcard() {
        XCTAssertTrue(matches("https://a.com/v1?token=*", "https://a.com/v1?token=abc123"))
    }

    func test_fragmentIsNeverMatched() {
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1#section"))
    }

    // MARK: - Methods

    func test_emptyMethodSetMatchesAnyMethod() {
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1", method: "POST"))
    }

    func test_methodFilterIsRespected() {
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1", method: "GET", methods: ["GET"]))
        XCTAssertFalse(matches("https://a.com/v1", "https://a.com/v1", method: "POST", methods: ["GET"]))
        XCTAssertTrue(matches("https://a.com/v1", "https://a.com/v1", method: "post", methods: ["POST"]))
    }

    // MARK: - Regex

    func test_regexIsAnchoredToTheWholeURL() {
        XCTAssertTrue(matches(".*/product-state", "https://a.com/v1/product-state", kind: .regex))
        XCTAssertFalse(matches("/product-state", "https://a.com/v1/product-state", kind: .regex))
    }

    func test_invalidRegexDoesNotCompileAndNeverMatches() {
        let r = OverrideRule(matcher: OverrideMatcher(pattern: "([", kind: .regex))
        let set = OverrideCompiler.compile([r], masterEnabled: true)
        XCTAssertTrue(set.rules.isEmpty)
        XCTAssertNotNil(set.diagnostics[r.id])
    }

    // MARK: - Route-host derivation

    func test_literalHostIsDerivedFromPattern() {
        XCTAssertEqual(OverrideCompiler.derivedRoutedHosts(
            for: OverrideMatcher(pattern: "https://api.teya.xyz/lending/**")), ["api.teya.xyz"])
        XCTAssertEqual(OverrideCompiler.derivedRoutedHosts(
            for: OverrideMatcher(pattern: "api.teya.xyz/lending/**")), ["api.teya.xyz"])
    }

    /// A wildcarded host must yield **empty**, never "all hosts" — that emptiness is what makes
    /// the editor ask the user which hosts to route, so Jaca never tunnels everything by accident.
    func test_wildcardHostDerivesNoRoutedHosts() {
        XCTAssertTrue(OverrideCompiler.derivedRoutedHosts(
            for: OverrideMatcher(pattern: "**/product-state")).isEmpty)
        XCTAssertTrue(OverrideCompiler.derivedRoutedHosts(
            for: OverrideMatcher(pattern: "*.teya.xyz/v1/**")).isEmpty)
        XCTAssertTrue(OverrideCompiler.derivedRoutedHosts(
            for: OverrideMatcher(pattern: ".*", kind: .regex)).isEmpty)
    }

    // MARK: - Generalize

    func test_generalizeReplacesUUIDAndNumericSegments() {
        XCTAssertEqual(
            OverrideMatching.generalize("https://a.com/v1/companies/3F2504E0-4F89-11D3-9A0C-0305E82C3301/state"),
            "**/v1/companies/*/state")
        XCTAssertEqual(OverrideMatching.generalize("https://a.com/users/12345/posts"),
                       "**/users/*/posts")
    }

    func test_generalizeLeavesRouteSegmentsAlone() {
        XCTAssertEqual(OverrideMatching.generalize("https://a.com/lending/v1/product-state"),
                       "**/lending/v1/product-state")
    }

    /// The base URL goes too: scheme, host and port become `**`, and a pattern that already
    /// starts with `**` is left as it is (the path is still generalized).
    func test_generalizeDropsTheBaseURL() {
        XCTAssertEqual(OverrideMatching.generalize("http://localhost:3000/v1/users/42"), "**/v1/users/*")
        XCTAssertEqual(OverrideMatching.generalize("a.com/v1/state"), "**/v1/state")
        XCTAssertEqual(OverrideMatching.generalize("**/v1/users/42"), "**/v1/users/*")
        XCTAssertEqual(OverrideMatching.generalize("**/v1/state"), "**/v1/state", "idempotent")
        XCTAssertEqual(OverrideMatching.generalize("https://a.com"), "https://a.com", "nothing to generalize")
    }

    /// The command line's rewrite: only the base URL changes, an id the pattern names stays.
    func test_anyBaseURLKeepsThePathAsWritten() {
        XCTAssertEqual(OverrideMatching.anyBaseURL("https://a.com/v1/companies/*/deals/c011ec71-0000-4000-a000-000000000001"),
                       "**/v1/companies/*/deals/c011ec71-0000-4000-a000-000000000001")
        XCTAssertEqual(OverrideMatching.anyBaseURL("http://localhost:3000/v1/users/42"), "**/v1/users/42")
        XCTAssertEqual(OverrideMatching.anyBaseURL("**/v1/users/42"), "**/v1/users/42", "idempotent")
        XCTAssertEqual(OverrideMatching.anyBaseURL("https://a.com"), "https://a.com", "no path to keep")
    }

    /// Why the base URL is dropped: a rule made from the device's capture must also answer the
    /// portal, whose dev server proxies the same endpoint under `localhost` and a path prefix.
    func test_generalizedPatternMatchesTheSameEndpointOnAnyBaseURL() {
        let device = "https://private-api.teya.xyz/lending/v1/companies/7c1f3c2e-2b1a-4c2d-9e1f-0a1b2c3d4e5f/deals"
        let pattern = OverrideMatching.generalize(device)
        XCTAssertEqual(pattern, "**/lending/v1/companies/*/deals")

        let program = try! OverrideMatching.compileGlob(pattern).get()
        let compiled = CompiledRule(rule: OverrideRule(matcher: OverrideMatcher(pattern: pattern)), program: program, regex: nil)
        let portal = "http://localhost:3000/private-api/lending/v1/companies/0f0e0d0c-0b0a-4908-8706-050403020100/deals?page=1"
        XCTAssertTrue(OverrideMatching.matches(compiled, OverrideMatching.facts(url: device)!, method: "GET"))
        XCTAssertTrue(OverrideMatching.matches(compiled, OverrideMatching.facts(url: portal)!, method: "GET"))
        XCTAssertFalse(OverrideMatching.matches(compiled, OverrideMatching.facts(url: "https://private-api.teya.xyz/lending/v1/deals")!,
                                                method: "GET"), "still anchored on the path")
    }
}
