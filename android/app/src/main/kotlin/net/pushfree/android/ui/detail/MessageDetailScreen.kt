package net.pushfree.android.ui.detail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Info
import androidx.compose.material3.AssistChip
import androidx.compose.material3.AssistChipDefaults
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import net.pushfree.android.notifications.HtmlSanitizer

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MessageDetailScreen(
    viewModel: MessageDetailViewModel,
    onBack: () -> Unit,
    onAcknowledge: ((String) -> Unit)? = null,
) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    MessageDetailContent(state = state, onBack = onBack, onAcknowledge = onAcknowledge)
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MessageDetailContent(
    state: MessageDetailUiState,
    onBack: () -> Unit,
    onAcknowledge: ((String) -> Unit)? = null,
) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(state.title ?: "Message") },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
            )
        },
    ) { padding ->
        val message = state.message
        if (message == null) {
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(padding),
                contentAlignment = Alignment.Center,
            ) {
                CircularProgressIndicator()
            }
        } else {
            Column(
                modifier = Modifier
                    .fillMaxSize()
                    .padding(padding)
                    .verticalScroll(rememberScrollState())
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                PriorityChip(priority = state.priority, isEmergency = state.isEmergency)
                Text(
                    text = state.title ?: "Pushfree",
                    style = MaterialTheme.typography.headlineSmall,
                    fontWeight = FontWeight.SemiBold,
                )
                val attachmentUri = state.attachmentUri
                if (attachmentUri != null) {
                    AttachmentCard(uri = attachmentUri)
                }
                HtmlBody(body = state.body)
                val receiptId = state.receiptId
                if (state.canAcknowledge && receiptId != null && onAcknowledge != null) {
                    Button(
                        onClick = { onAcknowledge(receiptId) },
                        modifier = Modifier.fillMaxWidth(),
                    ) {
                        Text("Acknowledge")
                    }
                }
            }
        }
    }
}

@Composable
private fun PriorityChip(priority: Int, isEmergency: Boolean) {
    val label: String
    val colors = when {
        isEmergency -> {
            label = "Emergency"
            AssistChipDefaults.assistChipColors(
                containerColor = MaterialTheme.colorScheme.errorContainer,
                labelColor = MaterialTheme.colorScheme.onErrorContainer,
            )
        }
        priority == 1 -> {
            label = "High"
            AssistChipDefaults.assistChipColors(
                containerColor = MaterialTheme.colorScheme.tertiaryContainer,
                labelColor = MaterialTheme.colorScheme.onTertiaryContainer,
            )
        }
        priority == 0 -> {
            label = "Normal"
            AssistChipDefaults.assistChipColors()
        }
        else -> {
            label = "Silent"
            AssistChipDefaults.assistChipColors(
                containerColor = MaterialTheme.colorScheme.surfaceVariant,
                labelColor = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
    }
    AssistChip(onClick = {}, label = { Text(label) }, colors = colors)
}

@Composable
private fun AttachmentCard(uri: String) {
    Surface(
        color = MaterialTheme.colorScheme.secondaryContainer,
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Row(
            modifier = Modifier.padding(16.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                Icons.Filled.Info,
                contentDescription = "Attachment",
                tint = MaterialTheme.colorScheme.onSecondaryContainer,
            )
            Spacer(Modifier.size(12.dp))
            Column {
                Text(
                    "Attachment",
                    style = MaterialTheme.typography.titleSmall,
                    color = MaterialTheme.colorScheme.onSecondaryContainer,
                    fontWeight = FontWeight.SemiBold,
                )
                Text(
                    uri,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSecondaryContainer,
                    maxLines = 2,
                )
            }
        }
    }
}

/**
 * Renders the message body as sanitized HTML through a platform TextView.
 * Sanitization runs before render, so only b/i/u/a survive and dangerous
 * schemes (javascript:) are stripped.
 */
@Composable
private fun HtmlBody(body: String) {
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant,
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        AndroidView(
            modifier = Modifier
                .fillMaxWidth()
                .padding(16.dp),
            factory = { ctx -> android.widget.TextView(ctx).apply { setTextIsSelectable(true) } },
            update = { tv -> tv.text = HtmlSanitizer.render(HtmlSanitizer.sanitize(body)) },
        )
    }
}
