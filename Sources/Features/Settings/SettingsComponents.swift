import SwiftUI
import Lemonade

/// One group of settings: a subtle Lemonade card with an overline title (the overline style
/// upper-cases it). No title gives a card with no header.
struct SettingsCard<Content: View>: View {
    var title: String?
    @ViewBuilder var content: () -> Content

    var body: some View {
        if let title {
            LemonadeUi.Card(contentPadding: .medium, background: .subtle,
                            header: CardHeaderConfig(title: title, headingStyle: .overline)) {
                stack
            }
        } else {
            LemonadeUi.Card(contentPadding: .medium, background: .subtle) { stack }
        }
    }

    private var stack: some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing300) {
            content()
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// One setting: the label, the control on the trailing edge, and the existing explainer folded
/// behind "Learn more" when the setting has one.
struct SettingsRow<Control: View>: View {
    let label: String
    var explainer: String?
    @ViewBuilder var control: () -> Control

    var body: some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing100) {
            HStack(alignment: .center, spacing: LemonadeTheme.spaces.spacing300) {
                SettingsLabel(text: label)
                    .frame(maxWidth: .infinity, alignment: .leading)
                control()
            }
            if let explainer {
                SettingsExplainer(text: explainer)
            }
        }
    }
}

/// A row that opens another sheet: the label and a trailing chevron, all one click target.
struct SettingsNavigationRow: View {
    let label: String
    let action: () -> Void
    @State private var hovering = false

    var body: some View {
        Button(action: action) {
            HStack(spacing: LemonadeTheme.spaces.spacing300) {
                SettingsLabel(text: label)
                    .frame(maxWidth: .infinity, alignment: .leading)
                LemonadeUi.Icon(icon: .chevronRight, contentDescription: nil, size: .small,
                                tint: LemonadeTheme.colors.content.contentSecondary)
                    .offset(x: hovering ? 2 : 0)
            }
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .onHover { hovering = $0 }
        .animation(.easeInOut(duration: 0.15), value: hovering)
    }
}

struct SettingsLabel: View {
    let text: String

    var body: some View {
        LemonadeUi.Text(text, textStyle: LemonadeTypography.shared.bodySmallSemiBold,
                        color: LemonadeTheme.colors.content.contentPrimary)
            .fixedSize(horizontal: false, vertical: true)
    }
}

/// Tertiary text that wraps over the full width.
struct SettingsCaption: View {
    let text: String

    var body: some View {
        LemonadeUi.Text(text, textStyle: LemonadeTypography.shared.bodyXSmallRegular,
                        color: LemonadeTheme.colors.content.contentTertiary)
            .fixedSize(horizontal: false, vertical: true)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// "Learn more" and the explainer it unfolds. The chevron turns to show the state; the label
/// stays the same. The fold resets each time Settings opens.
struct SettingsExplainer: View {
    let text: String
    @State private var expanded = false

    var body: some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing100) {
            Button(action: { withAnimation(.easeInOut(duration: 0.2)) { expanded.toggle() } }) {
                HStack(spacing: LemonadeTheme.spaces.spacing50) {
                    LemonadeUi.Text("Learn more", textStyle: LemonadeTypography.shared.bodyXSmallMedium,
                                    color: LemonadeTheme.colors.content.contentBrand)
                    LemonadeUi.Icon(icon: .chevronRight, contentDescription: nil, size: .xSmall,
                                    tint: LemonadeTheme.colors.content.contentBrand)
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityAddTraits(expanded ? [.isButton, .isSelected] : .isButton)

            if expanded {
                SettingsCaption(text: text)
                    .transition(.opacity.combined(with: .move(edge: .top)))
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .clipped()
    }
}

/// `LemonadeUi.Switch` drawn as a switch: on macOS its native Toggle otherwise draws a checkbox.
struct SettingsSwitch: View {
    let checked: Bool
    let onCheckedChange: (Bool) -> Void

    var body: some View {
        LemonadeUi.Switch(checked: checked, onCheckedChange: onCheckedChange)
            .toggleStyle(.switch)
            .controlSize(.small)
    }
}

/// A one-line monospaced text field built from Lemonade tokens. `LemonadeUi.TextField` is about
/// 56 pt tall on macOS and wraps a long placeholder, which a path or a rule value would hit.
struct SettingsTextField: View {
    let placeholder: String
    @Binding var text: String
    @FocusState private var focused: Bool

    var body: some View {
        TextField("", text: $text, prompt: Text(placeholder)
            .foregroundStyle(LemonadeTheme.colors.content.contentTertiary))
            .textFieldStyle(.plain)
            .font(.system(size: 12, design: .monospaced))
            .foregroundStyle(LemonadeTheme.colors.content.contentPrimary)
            .lineLimit(1)
            .focused($focused)
            .padding(.horizontal, LemonadeTheme.spaces.spacing300)
            .frame(height: LemonadeTheme.sizes.size800)
            .background(
                RoundedRectangle(cornerRadius: LemonadeTheme.radius.radius300, style: .continuous)
                    .fill(LemonadeTheme.colors.background.bgDefault)
            )
            .overlay(
                RoundedRectangle(cornerRadius: LemonadeTheme.radius.radius300, style: .continuous)
                    .strokeBorder(focused ? LemonadeTheme.colors.border.borderSelected
                                          : LemonadeTheme.colors.border.borderNeutralMedium,
                                  lineWidth: LemonadeTheme.borderWidth.base.border40)
            )
            .animation(.easeInOut(duration: 0.15), value: focused)
    }
}
