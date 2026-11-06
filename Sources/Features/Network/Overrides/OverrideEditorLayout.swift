import CoreGraphics

/// Geometry for the override editor sheet, kept out of the view so the clamps are testable.
///
/// A macOS `.sheet` has no resize control, so the frame the content asks for *is* the dialog. It has
/// to fit the display it opens on: too tall and the footer, Save with it, goes off-screen.
enum OverrideEditorLayout {
    static let idealSheetWidth: CGFloat = 1120
    static let minSheetWidth: CGFloat = 980
    static let idealSheetHeight: CGFloat = 780
    static let minSheetHeight: CGFloat = 560
    /// Room left around the sheet on a small display.
    static let screenMargin: CGFloat = 80

    /// The settings column: just wide enough for the six method chips on one line beside the label
    /// column, so everything else goes to the response pane. At 528 "DELETE" wrapped to a second line.
    static let settingsWidth: CGFloat = 548
    /// Fits "Delay (ms)"; "Hosts to route" wraps to two lines. Fixed rather than "as wide as the
    /// widest label", which would widen the column when that row appears and shift every control
    /// sideways mid-edit.
    static let labelWidth: CGFloat = 76

    /// `nil` means no screen could be read; that falls back to the ideal rather than guessing small.
    static func sheetHeight(visibleHeight: CGFloat?) -> CGFloat {
        clamp(visible: visibleHeight, ideal: idealSheetHeight, minimum: minSheetHeight)
    }

    static func sheetWidth(visibleWidth: CGFloat?) -> CGFloat {
        clamp(visible: visibleWidth, ideal: idealSheetWidth, minimum: minSheetWidth)
    }

    /// The part of the screen a sheet hung from a window can use: it drops from the window's content
    /// top and is centred on the window, so a short window low on the screen, or one flush with a
    /// screen edge, has less room than the screen. Feed each side to `sheetHeight`/`sheetWidth` in
    /// place of the screen's. Falls back to the screen when the window gives no usable room, and to
    /// `nil` when there's no screen to measure.
    static func roomBelowWindow(contentTop: CGFloat, windowMidX: CGFloat,
                                visible: CGRect) -> (width: CGFloat?, height: CGFloat?) {
        guard !visible.isEmpty else { return (nil, nil) }
        let height = min(visible.height, contentTop - visible.minY)
        let width = min(visible.width, 2 * min(windowMidX - visible.minX, visible.maxX - windowMidX))
        return (width > 0 ? width : visible.width, height > 0 ? height : visible.height)
    }

    private static func clamp(visible: CGFloat?, ideal: CGFloat, minimum: CGFloat) -> CGFloat {
        guard let visible, visible.isFinite, visible > 0 else { return ideal }
        return min(ideal, max(minimum, visible - screenMargin))
    }
}
