import XCTest
@testable import Jaca

final class SettingsPaneTests: XCTestCase {
    func test_tabOrder_matchesTheTabStrip() {
        XCTAssertEqual(SettingsPane.allCases.map(\.title),
                       ["General", "Devices", "Network", "Logs", "Diagnostics"])
    }

    func test_initIndex_roundTripsEveryPane() {
        for (index, pane) in SettingsPane.allCases.enumerated() {
            XCTAssertEqual(SettingsPane(index: index), pane)
        }
    }

    func test_initIndex_outOfRange_fallsBackToGeneral() {
        XCTAssertEqual(SettingsPane(index: -1), .general)
        XCTAssertEqual(SettingsPane(index: SettingsPane.allCases.count), .general)
    }

    func test_opening_prefersTheRequestedPane() {
        XCTAssertEqual(SettingsPane.opening(.network, last: .logs), .network)
    }

    func test_opening_withoutARequest_reopensTheLastPane() {
        XCTAssertEqual(SettingsPane.opening(nil, last: .logs), .logs)
    }
}
