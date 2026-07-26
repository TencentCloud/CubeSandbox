package com.example.cubesandbox;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.IOException;
import java.nio.file.AtomicMoveNotSupportedException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.Callable;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicBoolean;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.springframework.http.HttpStatus;
import org.springframework.web.server.ResponseStatusException;

class TaskStateTest {
    @TempDir
    Path tempDir;

    private final ObjectMapper objectMapper = new ObjectMapper();

    @Test
    void createTaskPersistsStateAndCanReadItBack() throws Exception {
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        Map<String, Object> created =
                taskState.createTask(new CreateTaskRequest("ship demo", Map.of("priority", "high")));
        Map<String, Object> loaded = taskState.getTask((String) created.get("id"));

        assertThat(loaded).containsEntry("title", "ship demo");
        assertThat(loaded.get("payload")).isEqualTo(Map.of("priority", "high"));
        assertThat(loaded).containsEntry("persisted", true);
        assertThat(loaded).doesNotContainKey("stateFile");
        assertThat(Files.exists(tempDir.resolve("tasks.json"))).isTrue();
    }

    @Test
    void createTaskDefaultsBlankTitle() {
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        Map<String, Object> created = taskState.createTask(new CreateTaskRequest("   ", null));

        assertThat(created).containsEntry("title", "demo task");
    }

    @Test
    void getTaskReturnsNotFoundForMissingId() {
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        assertThatThrownBy(() -> taskState.getTask("missing"))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.NOT_FOUND));
    }

    @Test
    void getTaskReturnsNotFoundForExpiredTask() throws Exception {
        writeTasks(Map.of("expired", taskRow("expired", Instant.now().minusSeconds(13 * 60 * 60))));
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        assertThatThrownBy(() -> taskState.getTask("expired"))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.NOT_FOUND));
    }

    @Test
    void getTaskReturnsNotFoundForMalformedCreatedAtValues() throws Exception {
        Map<String, Map<String, Object>> tasks = new LinkedHashMap<>();
        tasks.put("numeric-created-at", taskRowWithCreatedAt("numeric-created-at", 123));
        tasks.put("invalid-created-at", taskRowWithCreatedAt("invalid-created-at", "not-an-instant"));
        writeTasks(tasks);
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        assertThatThrownBy(() -> taskState.getTask("numeric-created-at"))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.NOT_FOUND));
        assertThatThrownBy(() -> taskState.getTask("invalid-created-at"))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.NOT_FOUND));
    }

    @Test
    void createTaskPrunesExpiredStateRows() throws Exception {
        Path stateFile = tempDir.resolve("tasks.json");
        Map<String, Map<String, Object>> tasks = new LinkedHashMap<>();
        tasks.put(
                "old",
                new LinkedHashMap<>(
                        Map.of(
                                "id", "old",
                                "title", "old task",
                                "createdAt", Instant.now().minusSeconds(60 * 60 * 24).toString())));
        objectMapper.writeValue(stateFile.toFile(), tasks);

        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);
        taskState.createTask(new CreateTaskRequest("new task", null));

        Map<String, Object> persisted = objectMapper.readValue(stateFile.toFile(), new TypeReference<>() {});
        assertThat(persisted).doesNotContainKey("old");
        assertThat(persisted).hasSize(1);
    }

    @Test
    void createTaskPrunesMalformedCreatedAtRows() throws Exception {
        Map<String, Map<String, Object>> tasks = new LinkedHashMap<>();
        tasks.put("numeric-created-at", taskRowWithCreatedAt("numeric-created-at", 123));
        tasks.put("invalid-created-at", taskRowWithCreatedAt("invalid-created-at", "not-an-instant"));
        writeTasks(tasks);

        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);
        taskState.createTask(new CreateTaskRequest("new task", null));

        Map<String, Map<String, Object>> persisted = readTasks();
        assertThat(persisted).doesNotContainKeys("numeric-created-at", "invalid-created-at");
        assertThat(persisted).hasSize(1);
    }

    @Test
    void createTaskEvictsOldestRowAtOneHundredTaskBoundary() throws Exception {
        Map<String, Map<String, Object>> tasks = new LinkedHashMap<>();
        Instant createdAt = Instant.now().minusSeconds(60);
        for (int index = 0; index < 100; index++) {
            String id = "task-%03d".formatted(index);
            tasks.put(id, taskRow(id, createdAt.plusSeconds(index)));
        }
        writeTasks(tasks);

        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);
        Map<String, Object> created = taskState.createTask(new CreateTaskRequest("newest", null));

        Map<String, Map<String, Object>> persisted = readTasks();
        assertThat(persisted).hasSize(100);
        assertThat(persisted).doesNotContainKey("task-000");
        assertThat(persisted).containsKeys("task-001", "task-099", (String) created.get("id"));
    }

    @Test
    void getTaskWrapsStateReadFailureAsInternalServerError() throws Exception {
        Files.createDirectory(tempDir.resolve("tasks.json"));
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        assertThatThrownBy(() -> taskState.getTask("task-1"))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.INTERNAL_SERVER_ERROR));
    }

    @Test
    void createTaskWrapsStateWriteFailureAsInternalServerError() throws Exception {
        Path blockedStateDir = tempDir.resolve("state-as-file");
        Files.writeString(blockedStateDir, "not a directory");
        TaskState taskState = TaskState.forStateDir(objectMapper, blockedStateDir);

        assertThatThrownBy(() -> taskState.createTask(new CreateTaskRequest("blocked", null)))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.INTERNAL_SERVER_ERROR));
    }

    @Test
    void createTaskFallsBackWhenAtomicMoveIsUnsupported() throws Exception {
        AtomicBoolean attemptedAtomicMove = new AtomicBoolean(false);
        TaskState taskState = TaskState.forStateDir(
                objectMapper,
                tempDir,
                (source, target, options) -> {
                    if (Arrays.asList(options).contains(StandardCopyOption.ATOMIC_MOVE)) {
                        attemptedAtomicMove.set(true);
                        throw new AtomicMoveNotSupportedException(
                                source.toString(), target.toString(), "test filesystem does not support atomic moves");
                    }
                    Files.move(source, target, options);
                });

        Map<String, Object> created = taskState.createTask(new CreateTaskRequest("fallback", null));

        assertThat(attemptedAtomicMove).isTrue();
        assertThat(readTasks()).containsKey((String) created.get("id"));
    }

    @Test
    void createTaskCleansUpTemporaryFileWhenMoveFails() throws Exception {
        TaskState taskState = TaskState.forStateDir(
                objectMapper,
                tempDir,
                (source, target, options) -> {
                    throw new IOException("forced move failure");
                });

        assertThatThrownBy(() -> taskState.createTask(new CreateTaskRequest("failed write", null)))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.INTERNAL_SERVER_ERROR));

        try (var files = Files.list(tempDir)) {
            assertThat(files.map(path -> path.getFileName().toString()))
                    .noneMatch(name -> name.endsWith(".json.tmp"));
        }
    }

    @Test
    void concurrentCreatesAndReadsDoNotLoseOrCorruptTasks() throws Exception {
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);
        ExecutorService executor = Executors.newFixedThreadPool(8);
        List<Callable<Map<String, Object>>> operations = new ArrayList<>();
        for (int index = 0; index < 40; index++) {
            int taskNumber = index;
            operations.add(
                    () -> {
                        Map<String, Object> created = taskState.createTask(
                                new CreateTaskRequest("task-" + taskNumber, Map.of("index", taskNumber)));
                        return taskState.getTask((String) created.get("id"));
                    });
        }

        try {
            List<Future<Map<String, Object>>> results = executor.invokeAll(operations);
            for (Future<Map<String, Object>> result : results) {
                assertThat(result.get()).containsEntry("status", "created");
            }
        } finally {
            executor.shutdownNow();
        }

        assertThat(readTasks()).hasSize(40);
    }

    @Test
    void createTaskRejectsOversizedTitleAndPayload() {
        TaskState taskState = TaskState.forStateDir(objectMapper, tempDir);

        assertThatThrownBy(() -> taskState.createTask(new CreateTaskRequest("x".repeat(201), null)))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.BAD_REQUEST));
        assertThatThrownBy(
                        () -> taskState.createTask(
                                new CreateTaskRequest("large payload", Map.of("blob", "x".repeat(17_000)))))
                .isInstanceOfSatisfying(
                        ResponseStatusException.class,
                        ex -> assertThat(ex.getStatusCode()).isEqualTo(HttpStatus.BAD_REQUEST));
    }

    private Map<String, Object> taskRow(String id, Instant createdAt) {
        return new LinkedHashMap<>(
                Map.of("id", id, "title", id, "status", "created", "createdAt", createdAt.toString()));
    }

    private Map<String, Object> taskRowWithCreatedAt(String id, Object createdAt) {
        return new LinkedHashMap<>(
                Map.of("id", id, "title", id, "status", "created", "createdAt", createdAt));
    }

    private void writeTasks(Map<String, Map<String, Object>> tasks) throws Exception {
        objectMapper.writeValue(tempDir.resolve("tasks.json").toFile(), tasks);
    }

    private Map<String, Map<String, Object>> readTasks() throws Exception {
        return objectMapper.readValue(tempDir.resolve("tasks.json").toFile(), new TypeReference<>() {});
    }
}
