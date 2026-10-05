import Foundation

// MARK: - Wire formats (daemon)

extension InterceptArmingState: Codable {
    private enum CodingKeys: String, CodingKey { case state, appID, port, hosts, message }

    /// Tolerant: an unknown state from a newer build reads as `.idle` ("nothing armed here").
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let appID = try c.decodeIfPresent(String.self, forKey: .appID) ?? ""
        switch try c.decodeIfPresent(String.self, forKey: .state) {
        case "waitingForAgent": self = .waitingForAgent
        case "agentTooOld": self = .agentTooOld
        case "waitingForApp": self = .waitingForApp(appID: appID)
        case "detached": self = .detached(appID: appID)
        case "active":
            self = .active(port: try c.decodeIfPresent(Int.self, forKey: .port) ?? 0,
                           hosts: Set(try c.decodeIfPresent([String].self, forKey: .hosts) ?? []))
        case "failed": self = .failed(try c.decodeIfPresent(String.self, forKey: .message) ?? "")
        default: self = .idle
        }
    }

    func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        switch self {
        case .idle: try c.encode("idle", forKey: .state)
        case .waitingForAgent: try c.encode("waitingForAgent", forKey: .state)
        case .agentTooOld: try c.encode("agentTooOld", forKey: .state)
        case .waitingForApp(let appID):
            try c.encode("waitingForApp", forKey: .state)
            try c.encode(appID, forKey: .appID)
        case .detached(let appID):
            try c.encode("detached", forKey: .state)
            try c.encode(appID, forKey: .appID)
        case .active(let port, let hosts):
            try c.encode("active", forKey: .state)
            try c.encode(port, forKey: .port)
            try c.encode(hosts.sorted(), forKey: .hosts)
        case .failed(let message):
            try c.encode("failed", forKey: .state)
            try c.encode(message, forKey: .message)
        }
    }
}
