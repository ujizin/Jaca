import SwiftUI
import Lemonade
import AppKit

/// Right-hand detail for a selected transaction: overview, headers, request and
/// response bodies, and timing.
struct NetworkDetailView: View {
    let session: NetworkSession
    /// Owned by `NetworkSessionView`, so the chosen tab and body mode survive when a resize
    /// moves this pane between the side-by-side and the stacked split.
    @Binding var tab: DetailTab
    @Binding var bodyMode: BodyMode
    @State private var copiedCurl = false
    /// The detail pane width, measured once for the layout only: below `stackedHeadersWidth`
    /// each header shows its value under its name instead of beside a fixed 160 pt column.
    @State private var paneWidth: CGFloat = 0
    private static let stackedHeadersWidth: CGFloat = 460
    private var stackedHeaders: Bool { paneWidth > 0 && paneWidth < Self.stackedHeadersWidth }

    /// Read straight from the session so the pane follows the selection (and the bodies
    /// rehydrated after it) without keeping a second copy of it.
    private var transaction: NetworkTransaction? { session.selected }

    enum DetailTab: String, CaseIterable { case overview = "Overview", headers = "Headers",
        request = "Request", response = "Response", timing = "Timing" }
    enum BodyMode { case tree, raw }

    var body: some View {
        if let txn = transaction {
            VStack(spacing: 0) {
                tabBar
                Rectangle().fill(LemonadeTheme.colors.border.borderNeutralLow).frame(height: 1)
                ScrollView { content(for: txn).padding(LemonadeTheme.spaces.spacing300) }
                    .background(GeometryReader { geo in
                        Color.clear
                            .onAppear { paneWidth = geo.size.width }
                            .onChange(of: geo.size.width) { _, width in paneWidth = width }
                    })
                    .animation(.easeInOut(duration: 0.2), value: stackedHeaders)
            }
            .background(LemonadeTheme.colors.background.bgDefault)
        } else {
            VStack {
                Spacer()
                LemonadeUi.Text("Select a request to inspect it.",
                                textStyle: LemonadeTypography.shared.bodySmallRegular,
                                color: LemonadeTheme.colors.content.contentTertiary)
                Spacer()
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .background(LemonadeTheme.colors.background.bgDefault)
        }
    }

    /// One row when it fits; in a narrow pane the same chips and button wrap onto more rows
    /// instead of compressing their labels. A resize drives the switch, and the chips hold no
    /// state, so nothing is lost when it changes.
    private var tabBar: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: LemonadeTheme.spaces.spacing100) {
                tabChips
                Spacer(minLength: LemonadeTheme.spaces.spacing200)
                copyCurlButton
            }
            FlowLayout(spacing: LemonadeTheme.spaces.spacing100, lineSpacing: LemonadeTheme.spaces.spacing100) {
                tabChips
                copyCurlButton
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(.horizontal, LemonadeTheme.spaces.spacing300)
        .padding(.vertical, LemonadeTheme.spaces.spacing200)
        .background(LemonadeTheme.colors.background.bgElevated)
    }

    @ViewBuilder
    private var tabChips: some View {
        ForEach(DetailTab.allCases, id: \.self) { item in
            LemonadeUi.Chip(label: item.rawValue, selected: tab == item,
                            onChipClicked: { tab = item })
                .fixedSize()
        }
    }

    @ViewBuilder
    private var copyCurlButton: some View {
        if let txn = transaction {
            LemonadeUi.Button(
                label: copiedCurl ? "Copied" : "Copy cURL",
                onClick: { copyCurl(txn) },
                leadingIcon: copiedCurl ? .circleCheck : .copy,
                variant: .neutral, type: .subtle, size: .xSmall
            )
            .fixedSize()
            .help("Copy this request as a runnable curl command")
            .animation(.easeInOut(duration: 0.15), value: copiedCurl)
        }
    }

    @ViewBuilder
    private func content(for txn: NetworkTransaction) -> some View {
        switch tab {
        case .overview: overview(txn)
        case .headers:
            headerSection("Request Headers", txn.displayRequestHeaders)
            headerSection("Response Headers", txn.displayResponseHeaders)
        case .request:
            bodyView(data: txn.requestBody,
                     contentType: txn.requestHeaders.first { $0.name.lowercased() == "content-type" }?.value)
        case .response:
            bodyView(data: txn.responseBody, contentType: txn.responseContentType)
        case .timing: timing(txn)
        }
    }

    private func overview(_ txn: NetworkTransaction) -> some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing200) {
            field("URL", txn.url)
            field("Method", txn.method)
            field("Status", txn.error ?? (txn.statusCode.map(String.init) ?? "—"))
            field("Content-Type", txn.responseContentType ?? "—")
            field("Request size", NetworkFormatting.size(txn.requestBytes))
            field("Response size", NetworkFormatting.size(txn.responseBytes))
            field("Duration", NetworkFormatting.duration(txn.duration))
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func timing(_ txn: NetworkTransaction) -> some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing200) {
            field("Started", txn.startedAt.formatted(date: .omitted, time: .standard))
            field("Time to first byte", NetworkFormatting.duration(txn.ttfb))
            field("Finished", txn.finishedAt?.formatted(date: .omitted, time: .standard) ?? "—")
            field("Total duration", NetworkFormatting.duration(txn.duration))
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func headerSection(_ title: String, _ headers: [HeaderPair]) -> some View {
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing100) {
            HStack(spacing: LemonadeTheme.spaces.spacing200) {
                LemonadeUi.Text(title.uppercased(), textStyle: LemonadeTypography.shared.bodyXSmallOverline,
                                color: LemonadeTheme.colors.content.contentTertiary)
                Spacer(minLength: 0)
                if !headers.isEmpty {
                    LemonadeUi.IconButton(icon: .copy, contentDescription: "Copy \(title)",
                                          onClick: { copyHeaders(headers) }, size: .small)
                }
            }
            if headers.isEmpty {
                LemonadeUi.Text("—", textStyle: LemonadeTypography.shared.bodySmallRegular,
                                color: LemonadeTheme.colors.content.contentTertiary)
            } else {
                ForEach(headers) { header in
                    if stackedHeaders {
                        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing50) {
                            Text(header.name)
                                .foregroundStyle(LemonadeTheme.colors.content.contentSecondary)
                                .frame(maxWidth: .infinity, alignment: .leading)
                            Text(header.value)
                                .foregroundStyle(LemonadeTheme.colors.content.contentPrimary)
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .font(LogLevelStyle.mono(11))
                    } else {
                        HStack(alignment: .top, spacing: LemonadeTheme.spaces.spacing200) {
                            Text(header.name)
                                .foregroundStyle(LemonadeTheme.colors.content.contentSecondary)
                                .frame(width: 160, alignment: .leading)
                            Text(header.value)
                                .foregroundStyle(LemonadeTheme.colors.content.contentPrimary)
                                .textSelection(.enabled)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .font(LogLevelStyle.mono(11))
                    }
                }
            }
        }
        .padding(.bottom, LemonadeTheme.spaces.spacing300)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func copyCurl(_ txn: NetworkTransaction) {
        Task { @MainActor in
            // Ask the session, not CurlExport directly: an older transaction's body lives in
            // the on-disk cache and has to be loaded back before the command is worth copying.
            guard let command = await session.curlCommand(for: txn.id) else { return }
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(command, forType: .string)
            withAnimation(.easeInOut(duration: 0.15)) { copiedCurl = true }
            try? await Task.sleep(for: .seconds(1.2))
            withAnimation(.easeInOut(duration: 0.15)) { copiedCurl = false }
        }
    }

    private func copyHeaders(_ headers: [HeaderPair]) {
        let text = headers.map { "\($0.name): \($0.value)" }.joined(separator: "\n")
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
    }

    @ViewBuilder
    private func bodyView(data: Data?, contentType: String?) -> some View {
        let text = NetworkFormatting.bodyText(data, contentType: contentType)
        let json = JSONParse.parse(data)
        VStack(alignment: .leading, spacing: LemonadeTheme.spaces.spacing200) {
            HStack {
                if json != nil {
                    LemonadeUi.SegmentedControl(
                        properties: [.label("Tree"), .label("Raw")],
                        selectedTab: bodyMode == .tree ? 0 : 1,
                        size: .small,
                        onTabSelected: { bodyMode = $0 == 0 ? .tree : .raw }
                    )
                    .fixedSize()
                }
                Spacer()
                LemonadeUi.Button(label: "Copy", onClick: {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(text, forType: .string)
                }, leadingIcon: .copy, variant: .neutral, type: .subtle, size: .small)
            }
            if text.isEmpty {
                LemonadeUi.Text("No body.", textStyle: LemonadeTypography.shared.bodySmallRegular,
                                color: LemonadeTheme.colors.content.contentTertiary)
            } else if let json, bodyMode == .tree {
                JSONTreeView(value: json)
            } else {
                Text(text)
                    .font(LogLevelStyle.mono(11))
                    .foregroundStyle(LemonadeTheme.colors.content.contentPrimary)
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func field(_ label: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            LemonadeUi.Text(label.uppercased(), textStyle: LemonadeTypography.shared.bodyXSmallOverline,
                            color: LemonadeTheme.colors.content.contentTertiary)
            Text(value)
                .font(LogLevelStyle.mono(11))
                .foregroundStyle(LemonadeTheme.colors.content.contentPrimary)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}
