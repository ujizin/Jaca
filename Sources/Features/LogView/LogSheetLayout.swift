import CoreGraphics

/// Sheet sizes for the log sheets (copy format, history). A macOS sheet cannot be resized and
/// hangs below the title bar, so each one takes its ideal size, shrinks to fit the visible
/// screen, and stops at a minimum. Same rule as `OverrideEditorLayout`.
enum LogSheetLayout {
    /// Room left around the sheet on a small display.
    static let screenMargin: CGFloat = 80

    static let copyFormatWidth: CGFloat = 580
    static let copyFormatIdealHeight: CGFloat = 800
    static let copyFormatMinHeight: CGFloat = 480

    static let historyIdealWidth: CGFloat = 1200
    static let historyMinWidth: CGFloat = 900
    static let historyIdealHeight: CGFloat = 760
    static let historyMinHeight: CGFloat = 560

    static func copyFormatHeight(visibleHeight: CGFloat?) -> CGFloat {
        clamp(visible: visibleHeight, ideal: copyFormatIdealHeight, minimum: copyFormatMinHeight)
    }

    static func historySize(visible: CGSize?) -> CGSize {
        CGSize(width: clamp(visible: visible?.width, ideal: historyIdealWidth, minimum: historyMinWidth),
               height: clamp(visible: visible?.height, ideal: historyIdealHeight, minimum: historyMinHeight))
    }

    /// `nil` means no screen could be read; that falls back to the ideal rather than guessing small.
    static func clamp(visible: CGFloat?, ideal: CGFloat, minimum: CGFloat) -> CGFloat {
        guard let visible, visible.isFinite, visible > 0 else { return ideal }
        return min(ideal, max(minimum, visible - screenMargin))
    }
}
