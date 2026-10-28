import Foundation

/// How groups order and gate the rule list. The rule list is kept in display order, which is also
/// precedence: the rules of the first group, then the next group's, and the rules in no group
/// last. A rule whose `groupID` names no group counts as in no group.
enum OverrideGrouping {

    /// One run of the list: a group and its rules, or (`group` nil) the rules in no group.
    struct Section: Equatable {
        var group: OverrideGroup?
        var rules: [OverrideRule]
    }

    /// The rules in display order. Each section keeps the relative order the rules had.
    static func normalized(_ rules: [OverrideRule], groups: [OverrideGroup]) -> [OverrideRule] {
        sections(rules, groups: groups).flatMap(\.rules)
    }

    /// Every group (also an empty one), then the rules in no group. That last section is left
    /// out when it is empty.
    static func sections(_ rules: [OverrideRule], groups: [OverrideGroup]) -> [Section] {
        let known = Set(groups.map(\.id))
        var byGroup: [UUID: [OverrideRule]] = [:]
        var loose: [OverrideRule] = []
        for rule in rules {
            if let id = rule.groupID, known.contains(id) {
                byGroup[id, default: []].append(rule)
            } else {
                loose.append(rule)
            }
        }
        var result = groups.map { Section(group: $0, rules: byGroup[$0.id] ?? []) }
        if !loose.isEmpty { result.append(Section(group: nil, rules: loose)) }
        return result
    }

    /// The group a rule is in, or nil when it is in none (or its group is gone).
    static func group(of rule: OverrideRule, in groups: [OverrideGroup]) -> OverrideGroup? {
        guard let id = rule.groupID else { return nil }
        return groups.first { $0.id == id }
    }

    /// Whether the rule applies, master switch aside: it is on, and so is its group.
    static func isActive(_ rule: OverrideRule, groups: [OverrideGroup]) -> Bool {
        rule.enabled && (group(of: rule, in: groups)?.enabled ?? true)
    }

    /// The index to swap the rule at `index` with, to move it by `offset` inside its own section,
    /// or nil when that would cross into another section or off the list.
    static func moveTarget(in rules: [OverrideRule], groups: [OverrideGroup], index: Int, offset: Int) -> Int? {
        guard rules.indices.contains(index) else { return nil }
        // `offset` can come off the wire: no overflow trap.
        let (target, overflow) = index.addingReportingOverflow(offset)
        guard !overflow, rules.indices.contains(target) else { return nil }
        let section = group(of: rules[index], in: groups)?.id
        guard group(of: rules[target], in: groups)?.id == section else { return nil }
        return target
    }
}
