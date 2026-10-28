import XCTest
@testable import Jaca

/// Override groups: the order they give the list (which is precedence), the group switch, the
/// engine's group edits, and loading files saved before groups existed. The engine tests point
/// the library at a temporary directory (`JACA_OVERRIDES_DIR`), so the user's rules are never
/// touched.
@MainActor
final class OverrideGroupTests: XCTestCase {
    private var dir: URL!

    override func setUp() async throws {
        dir = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("ovg-\(UUID().uuidString.prefix(8))")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        setenv("JACA_OVERRIDES_DIR", dir.path, 1)
    }

    override func tearDown() async throws {
        unsetenv("JACA_OVERRIDES_DIR")
        try? FileManager.default.removeItem(at: dir)
    }

    private func rule(_ name: String, in group: OverrideGroup? = nil, enabled: Bool = true) -> OverrideRule {
        OverrideRule(name: name, enabled: enabled,
                     matcher: OverrideMatcher(pattern: "https://api.example.com/\(name)"),
                     action: .respond(OverrideResponseSpec(statusCode: 200, headers: [], body: .inline("{}"))),
                     groupID: group?.id)
    }

    // MARK: - Order

    func test_normalized_putsGroupsFirstInTheirOrder_andKeepsEachSectionsOrder() {
        let deals = OverrideGroup(name: "Deals"), users = OverrideGroup(name: "Users")
        let rules = [rule("a"), rule("u1", in: users), rule("d1", in: deals), rule("b"), rule("d2", in: deals)]
        let order = OverrideGrouping.normalized(rules, groups: [deals, users]).map(\.name)
        XCTAssertEqual(order, ["d1", "d2", "u1", "a", "b"])
    }

    func test_sections_listEmptyGroups_andTreatAMissingGroupAsNone() {
        let deals = OverrideGroup(name: "Deals"), empty = OverrideGroup(name: "Empty")
        let gone = OverrideGroup(name: "Gone")
        let sections = OverrideGrouping.sections([rule("d1", in: deals), rule("orphan", in: gone)], groups: [deals, empty])
        XCTAssertEqual(sections.map(\.group?.name), ["Deals", "Empty", nil])
        XCTAssertEqual(sections.map { $0.rules.map(\.name) }, [["d1"], [], ["orphan"]])
        XCTAssertEqual(OverrideGrouping.sections([], groups: []), [], "no groups, no rules: no sections")
    }

    func test_moveTarget_staysInsideTheSection() {
        let deals = OverrideGroup(name: "Deals")
        let rules = [rule("d1", in: deals), rule("d2", in: deals), rule("a"), rule("b")]
        let groups = [deals]
        XCTAssertEqual(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 0, offset: 1), 1)
        XCTAssertNil(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 1, offset: 1), "into the next section")
        XCTAssertNil(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 2, offset: -1), "into the group above")
        XCTAssertEqual(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 3, offset: -1), 2)
        XCTAssertNil(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 0, offset: -1), "off the list")
        XCTAssertNil(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 9, offset: 1), "no such rule")
        XCTAssertNil(OverrideGrouping.moveTarget(in: rules, groups: groups, index: 0, offset: .max), "no overflow trap")
    }

    // MARK: - The group switch

    func test_aGroupThatIsOff_leavesItsRulesOutOfTheCompiledSet() {
        var deals = OverrideGroup(name: "Deals")
        deals.enabled = false
        let rules = [rule("d1", in: deals), rule("a"), rule("off", enabled: false)]
        let set = OverrideCompiler.compile(rules, masterEnabled: true, groups: [deals])
        XCTAssertEqual(set.rules.map(\.rule.name), ["a"])
        XCTAssertFalse(OverrideGrouping.isActive(rules[0], groups: [deals]))
        XCTAssertTrue(OverrideGrouping.isActive(rules[0], groups: []), "a group that is gone gates nothing")

        deals.enabled = true
        XCTAssertEqual(OverrideCompiler.compile(rules, masterEnabled: true, groups: [deals]).rules.map(\.rule.name), ["d1", "a"])
    }

    // MARK: - Engine

    func test_engine_groupEdits_keepTheListInDisplayOrder_andSaveBothFiles() throws {
        let engine = OverridesEngine()
        engine.save(rule("a"))
        engine.save(rule("b"))
        let deals = OverrideGroup(name: "Deals")
        engine.saveGroup(deals)
        let b = try XCTUnwrap(engine.state.rules.last)
        engine.setGroup(deals.id, for: b.id)
        XCTAssertEqual(engine.state.rules.map(\.name), ["b", "a"], "a rule in a group comes before the rules in none")
        XCTAssertEqual(engine.state.rules.first?.groupID, deals.id)

        // A second engine reads what the first saved.
        let reread = OverridesEngine()
        // The file keeps whole seconds, so the dates differ.
        XCTAssertEqual(reread.state.groups.map(\.id), [deals.id])
        XCTAssertEqual(reread.state.groups.map(\.name), ["Deals"])
        XCTAssertEqual(reread.state.rules.map(\.name), ["b", "a"])

        engine.setEnabled(false, forGroup: deals)
        XCTAssertNil(engine.compiled.rules.first { $0.rule.name == "b" }, "the group is off")
        XCTAssertTrue(engine.state.rules.first { $0.name == "b" }?.enabled ?? false, "the rule keeps its own switch")

        engine.removeGroup(deals.id)
        XCTAssertEqual(engine.state.groups, [])
        XCTAssertEqual(engine.state.rules.map(\.name), ["b", "a"], "deleting a group keeps its rules")
        XCTAssertNil(engine.state.rules.first?.groupID)
    }

    func test_engine_moveGroup_movesItsRulesPrecedence() {
        let engine = OverridesEngine()
        let one = OverrideGroup(name: "One"), two = OverrideGroup(name: "Two")
        engine.saveGroup(one)
        engine.saveGroup(two)
        engine.save(rule("t", in: two))
        engine.save(rule("o", in: one))
        XCTAssertEqual(engine.state.rules.map(\.name), ["o", "t"])
        engine.moveGroup(two.id, by: -1)
        XCTAssertEqual(engine.state.groups.map(\.name), ["Two", "One"])
        XCTAssertEqual(engine.state.rules.map(\.name), ["t", "o"])
        engine.moveGroup(two.id, by: -1)
        XCTAssertEqual(engine.state.groups.map(\.name), ["Two", "One"], "already first")
    }

    func test_engine_setGroup_toAGroupThatIsGone_putsTheRuleInNone() throws {
        let engine = OverridesEngine()
        engine.save(rule("a"))
        let a = try XCTUnwrap(engine.state.rules.first)
        engine.setGroup(UUID(), for: a.id)
        XCTAssertNil(engine.state.rules.first?.groupID)
    }

    func test_engine_withNoGroups_writesNoGroupsFile() {
        let engine = OverridesEngine()
        engine.save(rule("a"))
        XCTAssertFalse(FileManager.default.fileExists(atPath: OverrideRuleStore.groupsURL.path))
    }

    /// A broken `groups.json` must not let the next save replace the library.
    func test_engine_anUnreadableGroupsFile_holdsSaves() throws {
        try Data("{not json".utf8).write(to: OverrideRuleStore.groupsURL)
        let engine = OverridesEngine()
        engine.save(rule("a"))
        XCTAssertEqual(try String(contentsOf: OverrideRuleStore.groupsURL, encoding: .utf8), "{not json")
        XCTAssertFalse(FileManager.default.fileExists(atPath: OverrideRuleStore.rulesURL.path))
    }

    // MARK: - Files from before groups

    func test_aRuleSavedBeforeGroups_loadsInNoGroup() throws {
        let old = #"[{"id":"6F9619FF-8B86-D011-B42D-00CF4FC964FF","name":"Old","enabled":true,"#
            + #""matcher":{"pattern":"https://a.com/x"},"createdAt":"2026-01-01T00:00:00Z"}]"#
        try Data(old.utf8).write(to: OverrideRuleStore.rulesURL)
        let rules = OverrideRuleStore.load()
        XCTAssertEqual(rules.map(\.name), ["Old"])
        XCTAssertNil(rules.first?.groupID)
        XCTAssertEqual(OverrideRuleStore.loadGroups(), [], "no groups file: no groups")
    }

    func test_aGroupWithOnlyAnID_decodesWithDefaults() throws {
        let group = try OverrideRuleStore.makeDecoder().decode(
            OverrideGroup.self, from: Data(#"{"id":"6F9619FF-8B86-D011-B42D-00CF4FC964FF"}"#.utf8))
        XCTAssertEqual(group.name, "")
        XCTAssertTrue(group.enabled)
    }

    func test_aStateFromADaemonBeforeGroups_decodesWithNoGroups() throws {
        let state = try JSONDecoder.daemon.decode(OverridesState.self, from: Data(#"{"rules":[],"masterEnabled":true}"#.utf8))
        XCTAssertEqual(state.groups, [])
    }

    func test_groupID_roundTripsThroughTheRulesFile() throws {
        let deals = OverrideGroup(name: "Deals")
        XCTAssertTrue(OverrideRuleStore.save([rule("d", in: deals)]))
        XCTAssertTrue(OverrideRuleStore.saveGroups([deals]))
        XCTAssertEqual(OverrideRuleStore.load().first?.groupID, deals.id)
        XCTAssertEqual(OverrideRuleStore.loadGroups().map(\.id), [deals.id])
    }
}

private extension OverridesEngine {
    func setEnabled(_ enabled: Bool, forGroup group: OverrideGroup) {
        var changed = group
        changed.enabled = enabled
        saveGroup(changed)
    }
}
