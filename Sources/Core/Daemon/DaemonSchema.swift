import Foundation

/// The shape of a method's params or result in `api.describe`, as a JSON Schema subset: `type`,
/// `format`, `title`, `enum`, `properties`, `required`, `items`, `additionalProperties`, `oneOf`.
///
/// `required` lists what a request must carry. Every other property may be left out of params,
/// and may be absent from a result.
struct DaemonSchema: Codable, Sendable, Equatable {
    var type: String?
    var format: String?
    /// The Swift property behind a shortened wire key (`LogLine` sends `s` for `seq`).
    var title: String?
    var values: [String]?
    var properties: [String: DaemonSchema]?
    var required: [String]?
    var oneOf: [DaemonSchema]?
    // A struct can't hold itself directly; a one-element array can.
    private var itemsBox: [DaemonSchema]?
    private var additionalBox: [DaemonSchema]?

    var items: DaemonSchema? {
        get { itemsBox?.first }
        set { itemsBox = newValue.map { [$0] } }
    }
    var additionalProperties: DaemonSchema? {
        get { additionalBox?.first }
        set { additionalBox = newValue.map { [$0] } }
    }

    init(type: String? = nil, format: String? = nil) {
        self.type = type
        self.format = format
    }

    static let any = DaemonSchema()
    static let null = DaemonSchema(type: "null")
    static let string = DaemonSchema(type: "string")
    static let integer = DaemonSchema(type: "integer")
    static let number = DaemonSchema(type: "number")
    static let boolean = DaemonSchema(type: "boolean")
    static let uuid = DaemonSchema(type: "string", format: "uuid")
    /// ISO-8601, as `JSONEncoder.daemon` writes dates.
    static let date = DaemonSchema(type: "string", format: "date-time")
    /// Base64, as `Data` is encoded.
    static let bytes = DaemonSchema(type: "string", format: "byte")
    /// `RPCEmpty`.
    static let empty = DaemonSchema(type: "object")

    static func array(_ items: DaemonSchema) -> DaemonSchema {
        var s = DaemonSchema(type: "array")
        s.items = items
        return s
    }

    /// An object with arbitrary keys (a dictionary).
    static func map(_ value: DaemonSchema) -> DaemonSchema {
        var s = DaemonSchema(type: "object")
        s.additionalProperties = value
        return s
    }

    static func enumeration(_ values: [String]) -> DaemonSchema {
        var s = DaemonSchema(type: "string")
        s.values = values
        return s
    }

    static func object(_ required: [String: DaemonSchema] = [:], optional: [String: DaemonSchema] = [:]) -> DaemonSchema {
        var s = DaemonSchema(type: "object")
        s.properties = required.merging(optional) { r, _ in r }
        s.required = required.isEmpty ? nil : required.keys.sorted()
        return s
    }

    static func oneOf(_ variants: [DaemonSchema]) -> DaemonSchema {
        var s = DaemonSchema()
        s.oneOf = variants
        return s
    }

    /// `schema`, or `null`.
    static func nullable(_ schema: DaemonSchema) -> DaemonSchema { oneOf([schema, .null]) }

    func titled(_ title: String) -> DaemonSchema {
        var s = self
        s.title = title
        return s
    }

    private enum CodingKeys: String, CodingKey {
        case type, format, title, values = "enum", properties, required, oneOf
        case items, additionalProperties
    }

    /// Tolerant: a schema from a newer daemon keeps what this build understands.
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        type = try? c.decodeIfPresent(String.self, forKey: .type)
        format = try? c.decodeIfPresent(String.self, forKey: .format)
        title = try? c.decodeIfPresent(String.self, forKey: .title)
        values = try? c.decodeIfPresent([String].self, forKey: .values)
        properties = try? c.decodeIfPresent([String: DaemonSchema].self, forKey: .properties)
        required = try? c.decodeIfPresent([String].self, forKey: .required)
        oneOf = try? c.decodeIfPresent([DaemonSchema].self, forKey: .oneOf)
        itemsBox = (try? c.decodeIfPresent(DaemonSchema.self, forKey: .items)).map { [$0] }
        additionalBox = (try? c.decodeIfPresent(DaemonSchema.self, forKey: .additionalProperties)).map { [$0] }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encodeIfPresent(type, forKey: .type)
        try c.encodeIfPresent(format, forKey: .format)
        try c.encodeIfPresent(title, forKey: .title)
        try c.encodeIfPresent(values, forKey: .values)
        try c.encodeIfPresent(properties, forKey: .properties)
        try c.encodeIfPresent(required, forKey: .required)
        try c.encodeIfPresent(oneOf, forKey: .oneOf)
        try c.encodeIfPresent(items, forKey: .items)
        try c.encodeIfPresent(additionalProperties, forKey: .additionalProperties)
    }
}

extension DaemonSchema {
    /// The schema as indented `name: type` lines, for `jaca … --help`. A `?` marks a property a
    /// request may leave out.
    func rendered(indent: Int = 0) -> [String] {
        let pad = String(repeating: "  ", count: indent)
        if let oneOf {
            // A union of scalars is already on the property's own line (`summary`).
            guard oneOf.contains(where: { $0.properties != nil || $0.oneOf != nil }) else { return [] }
            return oneOf.flatMap { variant -> [String] in
                let lines = variant.rendered(indent: indent + 1)
                return lines.isEmpty ? ["\(pad)| \(variant.summary)"] : ["\(pad)|"] + lines
            }
        }
        if let items, type == "array" { return items.rendered(indent: indent) }
        if let additionalProperties { return additionalProperties.rendered(indent: indent) }
        guard let properties else { return [] }
        let needed = Set(required ?? [])
        return properties.keys.sorted().flatMap { key -> [String] in
            let property = properties[key] ?? .any
            let name = (needed.contains(key) ? key : key + "?") + (property.title.map { " (\($0))" } ?? "")
            return ["\(pad)\(name): \(property.summary)"] + property.rendered(indent: indent + 1)
        }
    }

    /// One word (or a short union) for the type: `uuid`, `string[]`, `"glob" | "regex"`.
    var summary: String {
        if let values { return values.map { "\"\($0)\"" }.joined(separator: " | ") }
        if let oneOf {
            // A union of scalars reads on one line; objects are listed under it.
            let scalars = oneOf.allSatisfy { $0.properties == nil && $0.oneOf == nil }
            return scalars ? oneOf.map(\.summary).joined(separator: " | ") : "oneOf"
        }
        switch type {
        case "array": return (items?.summary ?? "any") + "[]"
        case "object":
            if let additionalProperties { return "{string: \(additionalProperties.summary)}" }
            return "object"
        case let type?: return format ?? type
        case nil: return "any"
        }
    }
}
