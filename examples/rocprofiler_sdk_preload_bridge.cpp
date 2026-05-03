// ROCprofiler-SDK preload tool for the real HIP workload demo.
// This is a regular ROCprofiler tool library registered via
// rocprofiler_force_configure(), not a direct post-init API user. That avoids
// the CONFIGURATION_LOCKED failure seen when trying to create contexts after
// SDK initialization.

#include <rocprofiler-sdk/buffer.h>
#include <rocprofiler-sdk/buffer_tracing.h>
#include <rocprofiler-sdk/callback_tracing.h>
#include <rocprofiler-sdk/context.h>
#include <rocprofiler-sdk/internal_threading.h>
#include <rocprofiler-sdk/registration.h>

#include <algorithm>
#include <atomic>
#include <cstdint>
#include <cstring>
#include <cstdio>
#include <cstdlib>
#include <fcntl.h>
#include <mutex>
#include <sstream>
#include <string>
#include <unordered_map>
#include <unistd.h>

namespace
{
struct KernelMeta
{
    std::string name = {};
    uint64_t    address = 0;
};

struct BridgeState
{
    rocprofiler_context_id_t         context = {.handle = 0};
    rocprofiler_buffer_id_t          buffer = {.handle = 0};
    rocprofiler_callback_thread_t    callback_thread = {.handle = 0};
    std::mutex                       mutex = {};
    int                              output_fd = -1;
    std::string                      output_path = {};
    std::unordered_map<uint64_t, KernelMeta> kernels = {};
    char                             fallback_kernel_name[256] = "flash_attn_decode_bf16_gfx11";
    bool                             debug = false;
};

BridgeState g_state = {};
std::atomic<uint64_t> g_total_headers{0};
std::atomic<uint64_t> g_runtime_headers{0};
std::atomic<uint64_t> g_runtime_ext_headers{0};
std::atomic<uint64_t> g_dispatch_headers{0};
std::atomic<uint64_t> g_runtime_init_headers{0};
std::atomic<uint64_t> g_code_object_records{0};
std::atomic<uint64_t> g_written_lines{0};
std::atomic<uint64_t> g_runtime_launch_emits{0};
std::atomic<uint64_t> g_dispatch_emits{0};

constexpr uint64_t kBufferSizeBytes = 1 << 16;
constexpr uint64_t kWatermarkBytes = kBufferSizeBytes / 4;

std::string
json_escape(const std::string& value)
{
    std::string out;
    out.reserve(value.size() + 8);
    for(char ch : value)
    {
        switch(ch)
        {
            case '\\': out += "\\\\"; break;
            case '"': out += "\\\""; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default: out += ch; break;
        }
    }
    return out;
}

std::string
kernel_name_from_env()
{
    if(const char* env = std::getenv("PERF_AGENT_GPU_KERNEL_NAME"); env && env[0] != '\0')
    {
        return env;
    }
    return "flash_attn_decode_bf16_gfx11";
}

std::string
fallback_kernel_name_value()
{
    return std::string{g_state.fallback_kernel_name};
}

std::string
display_kernel_name(const std::string& kernel_name)
{
    if(kernel_name.size() > 3 &&
       kernel_name.compare(kernel_name.size() - 3, 3, ".kd") == 0)
    {
        return kernel_name.substr(0, kernel_name.size() - 3);
    }
    return kernel_name;
}

void
log_error(const std::string& msg)
{
    std::fprintf(stderr, "[perf-agent sdk preload] %s\n", msg.c_str());
    std::fflush(stderr);
}

void
log_debug(const std::string& msg)
{
    if(!g_state.debug) return;
    std::fprintf(stderr, "[perf-agent sdk preload] debug: %s\n", msg.c_str());
    std::fflush(stderr);
}

bool
debug_enabled()
{
    if(const char* env = std::getenv("PERF_AGENT_ROCPROFILER_SDK_DEBUG"); env && env[0] != '\0' &&
                                                                              env[0] != '0')
    {
        return true;
    }
    return false;
}

void
write_line_locked(const std::string& line)
{
    const auto payload = line + "\n";

    if(g_state.output_fd >= 0)
    {
        const auto* data = payload.data();
        ssize_t remaining = static_cast<ssize_t>(payload.size());
        while(remaining > 0)
        {
            const auto written = ::write(g_state.output_fd, data, static_cast<size_t>(remaining));
            if(written < 0)
            {
                log_error("output fd write syscall failed");
                return;
            }
            remaining -= written;
            data += written;
        }
        if(::fsync(g_state.output_fd) != 0)
        {
            log_error("output fd fsync syscall failed");
            return;
        }
    }
    else if(!g_state.output_path.empty())
    {
        const auto fd = ::open(g_state.output_path.c_str(), O_WRONLY | O_APPEND | O_CREAT, 0644);
        if(fd < 0)
        {
            log_error(std::string{"failed to open output path for append: "} + g_state.output_path);
            return;
        }

        const auto* data = payload.data();
        ssize_t remaining = static_cast<ssize_t>(payload.size());
        while(remaining > 0)
        {
            const auto written = ::write(fd, data, static_cast<size_t>(remaining));
            if(written < 0)
            {
                ::close(fd);
                log_error("output write syscall failed");
                return;
            }
            remaining -= written;
            data += written;
        }
        if(::fsync(fd) != 0)
        {
            ::close(fd);
            log_error("output fsync syscall failed");
            return;
        }
        if(::close(fd) != 0)
        {
            log_error("output close syscall failed");
            return;
        }
    }
    else
    {
        log_debug("write_line_locked skipped because output target is empty");
        return;
    }
    auto written = g_written_lines.fetch_add(1) + 1;
    if(g_state.debug && written <= 8)
    {
        std::ostringstream os{};
        os << "wrote line[" << written << "] bytes=" << line.size();
        log_debug(os.str());
    }
}

std::string
kind_name(rocprofiler_buffer_tracing_kind_t kind)
{
    const char* name = nullptr;
    if(rocprofiler_query_buffer_tracing_kind_name(kind, &name, nullptr) ==
           ROCPROFILER_STATUS_SUCCESS &&
       name != nullptr)
    {
        return name;
    }
    return "unknown_kind";
}

std::string
operation_name(rocprofiler_buffer_tracing_kind_t kind, rocprofiler_tracing_operation_t operation)
{
    const char* name = nullptr;
    if(rocprofiler_query_buffer_tracing_kind_operation_name(kind, operation, &name, nullptr) ==
           ROCPROFILER_STATUS_SUCCESS &&
       name != nullptr)
    {
        return name;
    }
    return "unknown_operation";
}

void
debug_header(rocprofiler_buffer_tracing_kind_t kind,
             rocprofiler_tracing_operation_t   operation,
             uint64_t                          corr_id,
             int64_t                           start_ns,
             int64_t                           end_ns)
{
    if(!g_state.debug) return;
    auto seen = g_total_headers.fetch_add(1) + 1;
    if(seen > 24) return;

    std::ostringstream os{};
    os << "header[" << seen << "] kind=" << kind_name(kind) << " op="
       << operation_name(kind, operation) << " corr=" << corr_id << " start=" << start_ns
       << " end=" << end_ns;
    log_debug(os.str());
}

bool
check(rocprofiler_status_t status, const char* step)
{
    if(status == ROCPROFILER_STATUS_SUCCESS) return true;
    std::ostringstream os{};
    os << step << " failed with status " << static_cast<int>(status);
    log_error(os.str());
    return false;
}

void
code_object_callback(rocprofiler_callback_tracing_record_t record,
                     rocprofiler_user_data_t* /*user_data*/,
                     void* /*callback_data*/)
{
    if(record.kind != ROCPROFILER_CALLBACK_TRACING_CODE_OBJECT ||
       record.operation != ROCPROFILER_CODE_OBJECT_DEVICE_KERNEL_SYMBOL_REGISTER)
    {
        return;
    }

    auto* data =
        static_cast<rocprofiler_callback_tracing_code_object_kernel_symbol_register_data_t*>(
            record.payload);
    if(data == nullptr) return;
    g_code_object_records.fetch_add(1);

    std::lock_guard<std::mutex> lk{g_state.mutex};
    if(record.phase == ROCPROFILER_CALLBACK_PHASE_LOAD)
    {
        g_state.kernels[data->kernel_id] = KernelMeta{
            .name = (data->kernel_name != nullptr) ? std::string{data->kernel_name} : std::string{},
            .address = data->kernel_address.handle,
        };
    }
    else if(record.phase == ROCPROFILER_CALLBACK_PHASE_UNLOAD)
    {
        g_state.kernels.erase(data->kernel_id);
    }
}

template <typename RecordT>
void
emit_runtime_record(rocprofiler_buffer_tracing_kind_t kind, RecordT* record)
{
    if(record == nullptr) return;

    const auto start_ns = static_cast<int64_t>(record->start_timestamp);
    const auto end_ns = static_cast<int64_t>(record->end_timestamp);
    const auto sample_ns = (end_ns > start_ns) ? (start_ns + ((end_ns - start_ns) / 2)) : end_ns;
    const auto weight = std::max<int64_t>(1, std::max<int64_t>(1, end_ns - start_ns) / 1000);
    const auto corr_id = static_cast<uint64_t>(record->correlation_id.internal);
    const auto op_name = operation_name(kind, record->operation);
    const auto function_name = display_kernel_name(fallback_kernel_name_value());

    debug_header(kind, record->operation, corr_id, start_ns, end_ns);
    if(op_name.find("hipLaunchKernel") == std::string::npos) return;
    g_runtime_launch_emits.fetch_add(1);
    log_debug(std::string{"matched runtime launch op="} + op_name + " corr=" + std::to_string(corr_id));

    std::ostringstream dispatch_json{};
    dispatch_json << "{\"kind\":\"dispatch\""
                  << ",\"id\":\"hip-launch:" << corr_id << "\""
                  << ",\"dispatch\":{\"id\":\"hip-launch:" << corr_id << "\"}"
                  << ",\"start_ns\":" << start_ns
                  << ",\"end_ns\":" << end_ns
                  << ",\"kernel\":{\"name\":\"" << json_escape(fallback_kernel_name_value()) << "\"}"
                  << ",\"kernel_name\":\"" << json_escape(fallback_kernel_name_value()) << "\""
                  << "}";

    std::ostringstream sample_json{};
    sample_json << "{\"kind\":\"sample\""
                << ",\"dispatch_id\":\"hip-launch:" << corr_id << "\""
                << ",\"sample_id\":\"hip-launch-sample:" << corr_id << "\""
                << ",\"time_ns\":" << sample_ns
                << ",\"function\":\"" << json_escape(function_name) << "\""
                << ",\"stall\":{\"reason\":\"hip_launch_runtime\"}"
                << ",\"stall_reason\":\"hip_launch_runtime\""
                << ",\"weight\":" << weight
                << "}";

    std::lock_guard<std::mutex> lk{g_state.mutex};
    write_line_locked(dispatch_json.str());
    write_line_locked(sample_json.str());
}

void
dispatch_buffer_callback(rocprofiler_context_id_t /*context_id*/,
                         rocprofiler_buffer_id_t /*buffer_id*/,
                         rocprofiler_record_header_t** headers,
                         size_t num_headers,
                         void* /*user_data*/,
                         uint64_t /*drop_count*/)
{
    for(size_t i = 0; i < num_headers; ++i)
    {
        auto* header = headers[i];
        if(header == nullptr) continue;
        if(header->category != ROCPROFILER_BUFFER_CATEGORY_TRACING)
        {
            continue;
        }
        const auto tracing_kind = static_cast<rocprofiler_buffer_tracing_kind_t>(header->kind);

        if(tracing_kind == ROCPROFILER_BUFFER_TRACING_RUNTIME_INITIALIZATION)
        {
            auto* record = static_cast<rocprofiler_buffer_tracing_runtime_initialization_record_t*>(
                header->payload);
            if(record != nullptr)
            {
                g_runtime_init_headers.fetch_add(1);
                debug_header(tracing_kind,
                             record->operation,
                             record->correlation_id.internal,
                             static_cast<int64_t>(record->timestamp),
                             static_cast<int64_t>(record->timestamp));
            }
            continue;
        }

        if(tracing_kind == ROCPROFILER_BUFFER_TRACING_HIP_RUNTIME_API)
        {
            auto* record = static_cast<rocprofiler_buffer_tracing_hip_api_record_t*>(header->payload);
            g_runtime_headers.fetch_add(1);
            if(record != nullptr)
            {
                debug_header(tracing_kind,
                             record->operation,
                             record->correlation_id.internal,
                             static_cast<int64_t>(record->start_timestamp),
                             static_cast<int64_t>(record->end_timestamp));
            }
            continue;
        }

        if(tracing_kind == ROCPROFILER_BUFFER_TRACING_HIP_RUNTIME_API_EXT)
        {
            auto* record =
                static_cast<rocprofiler_buffer_tracing_hip_api_ext_record_t*>(header->payload);
            g_runtime_ext_headers.fetch_add(1);
            emit_runtime_record(tracing_kind, record);
            continue;
        }

        if(tracing_kind != ROCPROFILER_BUFFER_TRACING_KERNEL_DISPATCH) continue;

        auto* record =
            static_cast<rocprofiler_buffer_tracing_kernel_dispatch_record_t*>(header->payload);
        if(record == nullptr) continue;
        g_dispatch_headers.fetch_add(1);

        const auto dispatch_handle = static_cast<uint64_t>(record->dispatch_info.dispatch_id);
        const auto kernel_id = static_cast<uint64_t>(record->dispatch_info.kernel_id);
        const auto start_ns = static_cast<int64_t>(record->start_timestamp);
        const auto end_ns = static_cast<int64_t>(record->end_timestamp);
        const auto sample_ns = (end_ns > start_ns) ? (start_ns + ((end_ns - start_ns) / 2)) : end_ns;
        const auto duration_ns = std::max<int64_t>(1, end_ns - start_ns);
        const auto weight = std::max<int64_t>(1, duration_ns / 1000);
        debug_header(tracing_kind,
                     record->operation,
                     record->correlation_id.internal,
                     start_ns,
                     end_ns);

        std::string kernel_name = fallback_kernel_name_value();
        uint64_t kernel_pc = 0;
        {
            std::lock_guard<std::mutex> lk{g_state.mutex};
            if(auto itr = g_state.kernels.find(kernel_id); itr != g_state.kernels.end())
            {
                if(!itr->second.name.empty()) kernel_name = itr->second.name;
                kernel_pc = itr->second.address;
            }
        }

        const auto display_name = display_kernel_name(kernel_name);

        std::ostringstream dispatch_json{};
        dispatch_json << "{\"kind\":\"dispatch\""
                      << ",\"id\":\"dispatch:" << dispatch_handle << "\""
                      << ",\"dispatch\":{\"id\":\"dispatch:" << dispatch_handle << "\"}"
                      << ",\"start_ns\":" << start_ns
                      << ",\"end_ns\":" << end_ns
                      << ",\"kernel\":{\"name\":\"" << json_escape(kernel_name) << "\"}"
                      << ",\"kernel_name\":\"" << json_escape(kernel_name) << "\""
                      << "}";

        std::ostringstream sample_json{};
        sample_json << "{\"kind\":\"sample\""
                    << ",\"dispatch_id\":\"dispatch:" << dispatch_handle << "\""
                    << ",\"sample_id\":\"dispatch-sample:" << dispatch_handle << "\""
                    << ",\"time_ns\":" << sample_ns
                    << ",\"function\":\"" << json_escape(display_name) << "\""
                    << ",\"stall\":{\"reason\":\"dispatch_complete\"}"
                    << ",\"stall_reason\":\"dispatch_complete\""
                    << ",\"weight\":" << weight;
        if(kernel_pc != 0)
        {
            std::ostringstream pc_stream{};
            pc_stream << "0x" << std::hex << kernel_pc;
            sample_json << ",\"pc\":\"" << pc_stream.str() << "\""
                        << ",\"location\":{\"pc\":\"" << pc_stream.str() << "\",\"function\":\""
                        << json_escape(display_name) << "\"}";
        }
        sample_json << "}";
        g_dispatch_emits.fetch_add(1);

        std::lock_guard<std::mutex> lk{g_state.mutex};
        write_line_locked(dispatch_json.str());
        write_line_locked(sample_json.str());
    }
}

int
tool_init(rocprofiler_client_finalize_t /*fini_func*/, void* /*tool_data*/)
{
    if(const char* output_fd_env = std::getenv("PERF_AGENT_ROCPROFILER_SDK_OUTPUT_FD");
       output_fd_env != nullptr && output_fd_env[0] != '\0')
    {
        g_state.output_fd = std::atoi(output_fd_env);
    }
    const char* output_path = std::getenv("PERF_AGENT_ROCPROFILER_SDK_OUTPUT_PATH");
    if(g_state.output_fd < 0 && (output_path == nullptr || output_path[0] == '\0'))
    {
        log_error(
            "PERF_AGENT_ROCPROFILER_SDK_OUTPUT_FD or PERF_AGENT_ROCPROFILER_SDK_OUTPUT_PATH must be set");
        return -1;
    }

    g_state.debug = debug_enabled();
    {
        const auto kernel_name = kernel_name_from_env();
        std::snprintf(g_state.fallback_kernel_name,
                      sizeof(g_state.fallback_kernel_name),
                      "%s",
                      kernel_name.c_str());
    }
    if(g_state.output_fd >= 0)
    {
        log_debug(std::string{"tool_init output_fd="} + std::to_string(g_state.output_fd));
        if(::ftruncate(g_state.output_fd, 0) != 0 || ::lseek(g_state.output_fd, 0, SEEK_SET) < 0)
        {
            log_error("failed to reset output fd");
            return -1;
        }
    }
    else
    {
        g_state.output_path = output_path;
        log_debug(std::string{"tool_init output_path="} + output_path);
        const auto fd = ::open(g_state.output_path.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
        if(fd < 0)
        {
            log_error(std::string{"failed to open output path: "} + output_path);
            return -1;
        }
        ::close(fd);
    }

    if(!check(rocprofiler_create_context(&g_state.context), "rocprofiler_create_context")) return -1;
    if(!check(rocprofiler_create_buffer(g_state.context,
                                        kBufferSizeBytes,
                                        kWatermarkBytes,
                                        ROCPROFILER_BUFFER_POLICY_LOSSLESS,
                                        dispatch_buffer_callback,
                                        nullptr,
                                        &g_state.buffer),
              "rocprofiler_create_buffer"))
    {
        return -1;
    }
    if(!check(rocprofiler_create_callback_thread(&g_state.callback_thread),
              "rocprofiler_create_callback_thread"))
    {
        return -1;
    }
    if(!check(rocprofiler_assign_callback_thread(g_state.buffer, g_state.callback_thread),
              "rocprofiler_assign_callback_thread"))
    {
        return -1;
    }
    if(!check(rocprofiler_configure_callback_tracing_service(g_state.context,
                                                             ROCPROFILER_CALLBACK_TRACING_CODE_OBJECT,
                                                             nullptr,
                                                             0,
                                                             code_object_callback,
                                                             nullptr),
              "rocprofiler_configure_callback_tracing_service(code_object)"))
    {
        return -1;
    }
    if(!check(rocprofiler_configure_buffer_tracing_service(g_state.context,
                                                           ROCPROFILER_BUFFER_TRACING_RUNTIME_INITIALIZATION,
                                                           nullptr,
                                                           0,
                                                           g_state.buffer),
              "rocprofiler_configure_buffer_tracing_service(runtime_init)"))
    {
        return -1;
    }
    if(!check(rocprofiler_configure_buffer_tracing_service(g_state.context,
                                                           ROCPROFILER_BUFFER_TRACING_HIP_RUNTIME_API,
                                                           nullptr,
                                                           0,
                                                           g_state.buffer),
              "rocprofiler_configure_buffer_tracing_service(hip_runtime)"))
    {
        return -1;
    }
    if(!check(rocprofiler_configure_buffer_tracing_service(g_state.context,
                                                           ROCPROFILER_BUFFER_TRACING_HIP_RUNTIME_API_EXT,
                                                           nullptr,
                                                           0,
                                                           g_state.buffer),
              "rocprofiler_configure_buffer_tracing_service(hip_runtime_ext)"))
    {
        return -1;
    }
    if(!check(rocprofiler_configure_buffer_tracing_service(g_state.context,
                                                           ROCPROFILER_BUFFER_TRACING_KERNEL_DISPATCH,
                                                           nullptr,
                                                           0,
                                                           g_state.buffer),
              "rocprofiler_configure_buffer_tracing_service(kernel_dispatch)"))
    {
        return -1;
    }

    int valid_ctx = 0;
    if(!check(rocprofiler_context_is_valid(g_state.context, &valid_ctx), "rocprofiler_context_is_valid")) return -1;
    if(valid_ctx == 0)
    {
        log_error("rocprofiler context is invalid");
        return -1;
    }

    if(!check(rocprofiler_start_context(g_state.context), "rocprofiler_start_context")) return -1;
    return 0;
}

void
tool_fini(void* /*tool_data*/)
{
    log_debug("tool_fini begin");
    if(g_state.buffer.handle != 0) rocprofiler_flush_buffer(g_state.buffer);
    if(g_state.debug)
    {
        std::ostringstream os{};
        os << "tool_fini counters runtime=" << g_runtime_headers.load()
           << " runtime_ext=" << g_runtime_ext_headers.load()
           << " dispatch=" << g_dispatch_headers.load()
           << " runtime_init=" << g_runtime_init_headers.load()
           << " code_object=" << g_code_object_records.load()
           << " runtime_launch_emits=" << g_runtime_launch_emits.load()
           << " dispatch_emits=" << g_dispatch_emits.load()
           << " written_lines=" << g_written_lines.load();
        log_debug(os.str());
    }
}

extern "C" rocprofiler_tool_configure_result_t*
rocprofiler_configure(uint32_t /*version*/,
                      const char* /*runtime_version*/,
                      uint32_t priority,
                      rocprofiler_client_id_t* id)
{
    if(priority > 0) return nullptr;
    if(id != nullptr) id->name = "PerfAgentPreloadBridge";

    static auto cfg = rocprofiler_tool_configure_result_t{
        sizeof(rocprofiler_tool_configure_result_t), &tool_init, &tool_fini, nullptr};
    return &cfg;
}

void
setup()
{
    int status = 0;
    if(rocprofiler_is_initialized(&status) != ROCPROFILER_STATUS_SUCCESS) return;
    if(status == 0)
    {
        if(!check(rocprofiler_force_configure(&rocprofiler_configure), "rocprofiler_force_configure"))
        {
            return;
        }
    }
}

bool cfg_on_load = (setup(), true);
}  // namespace
