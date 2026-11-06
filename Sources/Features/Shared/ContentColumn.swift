import SwiftUI

/// The widest a maintenance-area list (Projects, Gradle, Xcode) grows. Past it the
/// content stays centred, so a row's name and its size/actions stay close together in a
/// very wide window.
let maintenanceContentMaxWidth: CGFloat = 1200

extension View {
    /// Caps the view at `maintenanceContentMaxWidth` and centres it in the space it gets.
    func maintenanceContentColumn() -> some View {
        frame(maxWidth: maintenanceContentMaxWidth)
            .frame(maxWidth: .infinity)
    }
}
