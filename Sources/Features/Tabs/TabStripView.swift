import SwiftUI
import Lemonade
import UniformTypeIdentifiers

/// Horizontal strip of session tabs. Each tab shows a running dot, an editable
/// display name (double-click to rename), a filter subtitle, and a close button.
/// Tabs are draggable to reorder; Shift+Tab cycles the selection.
struct TabStripView: View {
    @Bindable var model: AppModel
    @State private var editingID: UUID?
    @State private var editingText = ""
    @State private var draggingID: UUID?
    @FocusState private var nameFocused: Bool

    var body: some View {
        // The reader keeps the selected tab in view when a new tab opens or the selection
        // cycles to a tab past the visible edge.
        ScrollViewReader { proxy in
            ScrollView(.horizontal, showsIndicators: false) {
                HStack(spacing: LemonadeTheme.spaces.spacing100) {
                    ForEach(model.sessions, id: \.id) { session in
                        tab(session)
                            .opacity(draggingID == session.id ? 0.4 : 1)
                            .onDrag {
                                draggingID = session.id
                                return NSItemProvider(object: session.id.uuidString as NSString)
                            }
                            .onDrop(of: [.text],
                                    delegate: TabDropDelegate(targetID: session.id, model: model,
                                                              draggingID: $draggingID))
                    }
                }
                .padding(.horizontal, LemonadeTheme.spaces.spacing200)
                .padding(.vertical, LemonadeTheme.spaces.spacing100)
            }
            .onAppear { scrollToSelection(proxy, animated: false) }
            .onChange(of: model.selectedSessionID) { _, _ in scrollToSelection(proxy, animated: true) }
        }
        .background(LemonadeTheme.colors.background.bgElevated)
        .background {
            // Shift+Tab cycles tabs forward; ⌃⇧Tab cycles back.
            Button("") { model.cycleTab(forward: true) }
                .keyboardShortcut(.tab, modifiers: .shift).hidden()
            Button("") { model.cycleTab(forward: false) }
                .keyboardShortcut(.tab, modifiers: [.control, .shift]).hidden()
        }
    }

    @ViewBuilder
    private func tab(_ session: any WorkspaceTab) -> some View {
        let isSelected = session.id == model.selectedSessionID
        Button(action: { model.select(session.id) }) {
            HStack(spacing: LemonadeTheme.spaces.spacing200) {
                Circle()
                    .fill(session.isRunning
                        ? LemonadeTheme.colors.content.contentPositive
                        : LemonadeTheme.colors.content.contentTertiary)
                    .frame(width: 7, height: 7)

                VStack(alignment: .leading, spacing: 0) {
                    if editingID == session.id {
                        TextField("Name", text: $editingText)
                            .textFieldStyle(.plain)
                            .font(LemonadeTypography.shared.bodySmallSemiBold.font)
                            .foregroundStyle(LemonadeTheme.colors.content.contentPrimary)
                            // Same height as the name label, so the strip doesn't shift on rename.
                            .frame(width: 130)
                            .frame(minHeight: LemonadeTypography.shared.bodySmallSemiBold.lineHeight)
                            .focused($nameFocused)
                            .onSubmit { commitRename(session) }
                            .onExitCommand { editingID = nil }
                    } else {
                        LemonadeUi.Text(
                            session.displayName,
                            textStyle: LemonadeTypography.shared.bodySmallSemiBold,
                            color: LemonadeTheme.colors.content.contentPrimary,
                            maxLines: 1
                        )
                    }
                    LemonadeUi.Text(
                        session.subtitle,
                        textStyle: LemonadeTypography.shared.bodyXSmallRegular,
                        color: LemonadeTheme.colors.content.contentTertiary,
                        maxLines: 1
                    )
                }
                // Cap the text column so a long subtitle truncates with "…" instead of stretching
                // the whole tab wide. A short name/subtitle still shrinks the tab to fit.
                .frame(maxWidth: 200, alignment: .leading)

                Button(action: { model.closeSession(session.id) }) {
                    Image(systemName: "xmark")
                        .font(.system(size: 9, weight: .bold))
                        .foregroundStyle(LemonadeTheme.colors.content.contentSecondary)
                        // A hit target larger than the 9 pt glyph.
                        .frame(width: LemonadeTheme.sizes.size400, height: LemonadeTheme.sizes.size400)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .help("Close tab")
                .accessibilityIdentifier("tabClose")
            }
            .padding(.horizontal, LemonadeTheme.spaces.spacing300)
            .padding(.vertical, LemonadeTheme.spaces.spacing200)
            .background(
                RoundedRectangle(cornerRadius: LemonadeTheme.radius.radius200)
                    .fill(isSelected
                        ? LemonadeTheme.colors.content.contentPositive.opacity(0.14)
                        : LemonadeTheme.colors.background.bgNeutralSubtle)
            )
            .overlay(
                RoundedRectangle(cornerRadius: LemonadeTheme.radius.radius200)
                    .stroke(isSelected
                        ? LemonadeTheme.colors.content.contentPositive
                        : LemonadeTheme.colors.border.borderNeutralMedium,
                        lineWidth: isSelected ? 1.5 : 1)
            )
            // Whole card is clickable, not just the text.
            .contentShape(RoundedRectangle(cornerRadius: LemonadeTheme.radius.radius200))
        }
        .buttonStyle(.plain)
        .simultaneousGesture(TapGesture(count: 2).onEnded { beginRename(session) })
        // Native macOS tooltip: shows the full name + context on hover without intercepting the
        // click (a SwiftUI .popover light-dismisses on click, which stole the tab's selection tap).
        .help(tooltipText(session))
        .contextMenu {
            Button("Rename") { beginRename(session) }
            Divider()
            Button("Close Tab") { model.closeSession(session.id) }
        }
    }

    private func scrollToSelection(_ proxy: ScrollViewProxy, animated: Bool) {
        guard let id = model.selectedSessionID else { return }
        if animated {
            withAnimation(.easeInOut(duration: 0.2)) { proxy.scrollTo(id) }
        } else {
            proxy.scrollTo(id)
        }
    }

    /// The hover tooltip text: the tab name, then its full context one part per line, so a
    /// truncated subtitle is fully readable.
    private func tooltipText(_ session: any WorkspaceTab) -> String {
        let context = session.subtitle.replacingOccurrences(of: " · ", with: "\n")
        return context.isEmpty ? session.displayName : "\(session.displayName)\n\(context)"
    }

    /// Starts an inline rename (also reachable by double-clicking the tab), focusing the field.
    private func beginRename(_ session: any WorkspaceTab) {
        editingText = session.displayName
        editingID = session.id
        DispatchQueue.main.async { nameFocused = true }
    }

    /// Commits the rename. The new name is stored on the session and persisted via its
    /// `onStateChanged` (so it survives relaunch, restored from the saved tab descriptor).
    private func commitRename(_ session: any WorkspaceTab) {
        let trimmed = editingText.trimmingCharacters(in: .whitespaces)
        if !trimmed.isEmpty { session.displayName = trimmed }
        editingID = nil
    }
}

/// Live-reorders tabs as the dragged one is hovered over another.
private struct TabDropDelegate: DropDelegate {
    let targetID: UUID
    let model: AppModel
    @Binding var draggingID: UUID?

    func dropEntered(info: DropInfo) {
        guard let dragging = draggingID, dragging != targetID else { return }
        withAnimation(.easeInOut(duration: 0.18)) {
            model.moveSession(dragging: dragging, over: targetID)
        }
    }
    func dropUpdated(info: DropInfo) -> DropProposal? { DropProposal(operation: .move) }
    func performDrop(info: DropInfo) -> Bool { draggingID = nil; return true }
}
