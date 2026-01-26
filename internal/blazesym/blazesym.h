/*
 * Please refer to the documentation hosted at
 *
 *   https://docs.rs/blazesym-c/0.1.5
 */


#ifndef __blazesym_h_
#define __blazesym_h_

#include <stdarg.h>
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

/**
 * An enum providing a rough classification of errors.
 *
 * C ABI compatible version of [`blazesym::ErrorKind`].
 */
typedef int16_t blaze_err;
/**
 * The operation was successful.
 */
#define BLAZE_ERR_OK 0
/**
 * An entity was not found, often a file.
 */
#define BLAZE_ERR_NOT_FOUND -2
/**
 * The operation lacked the necessary privileges to complete.
 */
#define BLAZE_ERR_PERMISSION_DENIED -1
/**
 * An entity already exists, often a file.
 */
#define BLAZE_ERR_ALREADY_EXISTS -17
/**
 * The operation needs to block to complete, but the blocking
 * operation was requested to not occur.
 */
#define BLAZE_ERR_WOULD_BLOCK -11
/**
 * Data not valid for the operation were encountered.
 */
#define BLAZE_ERR_INVALID_DATA -22
/**
 * The I/O operation's timeout expired, causing it to be canceled.
 */
#define BLAZE_ERR_TIMED_OUT -110
/**
 * This operation is unsupported on this platform.
 */
#define BLAZE_ERR_UNSUPPORTED -95
/**
 * An operation could not be completed, because it failed
 * to allocate enough memory.
 */
#define BLAZE_ERR_OUT_OF_MEMORY -12
/**
 * A parameter was incorrect.
 */
#define BLAZE_ERR_INVALID_INPUT -256
/**
 * An error returned when an operation could not be completed
 * because a call to [`write`][std::io::Write::write] returned
 * [`Ok(0)`][Ok].
 */
#define BLAZE_ERR_WRITE_ZERO -257
/**
 * An error returned when an operation ould not be completed
 * because an "end of file" was reached prematurely.
 */
#define BLAZE_ERR_UNEXPECTED_EOF -258
/**
 * DWARF input data was invalid.
 */
#define BLAZE_ERR_INVALID_DWARF -259
/**
 * A custom error that does not fall under any other I/O error
 * kind.
 */
#define BLAZE_ERR_OTHER -260

/**
 * The type of a symbol.
 */
typedef uint8_t blaze_sym_type;
/**
 * The symbol type is unspecified or unknown.
 *
 * In input contexts this variant can be used to encompass all
 * other variants (functions and variables), whereas in output
 * contexts it means that the type is not known.
 */
#define BLAZE_SYM_TYPE_UNDEF 0
/**
 * The symbol is a function.
 */
#define BLAZE_SYM_TYPE_FUNC 1
/**
 * The symbol is a variable.
 */
#define BLAZE_SYM_TYPE_VAR 2

/**
 * Information about a looked up symbol.
 */
typedef struct blaze_sym_info {
  /**
   * See [`inspect::SymInfo::name`].
   */
  const char *name;
  /**
   * See [`inspect::SymInfo::addr`].
   */
  uint64_t addr;
  /**
   * See [`inspect::SymInfo::size`].
   *
   * If the symbol's size is not available, this member will be `-1`.
   * Note that some symbol sources may not distinguish between
   * "unknown" size and `0`. In that case the size will be reported
   * as `0` here as well.
   */
  ptrdiff_t size;
  /**
   * See [`inspect::SymInfo::file_offset`].
   */
  uint64_t file_offset;
  /**
   * See [`inspect::SymInfo::module`].
   */
  const char *module;
  /**
   * See [`inspect::SymInfo::sym_type`].
   */
  blaze_sym_type sym_type;
  /**
   * Unused member available for future expansion.
   */
  uint8_t reserved[23];
} blaze_sym_info;

/**
 * C ABI compatible version of [`blazesym::inspect::Inspector`].
 */
typedef struct blaze_inspector blaze_inspector;

/**
 * An object representing an ELF inspection source.
 *
 * C ABI compatible version of [`inspect::source::Elf`].
 */
typedef struct blaze_inspect_elf_src {
  /**
   * The size of this object's type.
   *
   * Make sure to initialize it to `sizeof(<type>)`. This member is used to
   * ensure compatibility in the presence of member additions.
   */
  size_t type_size;
  /**
   * The path to the ELF file. This member is always present.
   */
  const char *path;
  /**
   * Whether or not to consult debug symbols to satisfy the request
   * (if present).
   */
  bool debug_syms;
  /**
   * Unused member available for future expansion. Must be initialized
   * to zero.
   */
  uint8_t reserved[23];
} blaze_inspect_elf_src;

/**
 * C ABI compatible version of [`blazesym::normalize::Normalizer`].
 */
typedef struct blaze_normalizer blaze_normalizer;

/**
 * Options for configuring [`blaze_normalizer`] objects.
 */
typedef struct blaze_normalizer_opts {
  /**
   * The size of this object's type.
   *
   * Make sure to initialize it to `sizeof(<type>)`. This member is used to
   * ensure compatibility in the presence of member additions.
   */
  size_t type_size;
  /**
   * Whether or not to use the `PROCMAP_QUERY` ioctl instead of
   * parsing `/proc/<pid>/maps` for getting available VMA ranges.
   */
  bool use_procmap_query;
  /**
   * Whether or not to cache `/proc/<pid>/maps` contents.
   */
  bool cache_vmas;
  /**
   * Whether to read and report build IDs as part of the normalization
   * process.
   */
  bool build_ids;
  /**
   * Whether or not to cache build IDs.
   */
  bool cache_build_ids;
  /**
   * Unused member available for future expansion. Must be initialized
   * to zero.
   */
  uint8_t reserved[20];
} blaze_normalizer_opts;

/**
 * The reason why normalization failed.
 */
typedef uint8_t blaze_normalize_reason;
#define BLAZE_NORMALIZE_REASON_UNMAPPED 0
#define BLAZE_NORMALIZE_REASON_MISSING_COMPONENT 1
#define BLAZE_NORMALIZE_REASON_UNSUPPORTED 2
#define BLAZE_NORMALIZE_REASON_INVALID_FILE_OFFSET 3
#define BLAZE_NORMALIZE_REASON_MISSING_SYMS 4
#define BLAZE_NORMALIZE_REASON_UNKNOWN_ADDR 5
#define BLAZE_NORMALIZE_REASON_IGNORED_ERROR 7

/**
 * The valid variant kind in [`blaze_user_meta`].
 */
typedef uint8_t blaze_user_meta_kind;
#define BLAZE_USER_META_KIND_UNKNOWN 0
#define BLAZE_USER_META_KIND_APK 1
#define BLAZE_USER_META_KIND_ELF 2
#define BLAZE_USER_META_KIND_SYM 3

/**
 * C compatible version of [`Apk`].
 */
typedef struct blaze_user_meta_apk {
  char *path;
  uint8_t reserved[16];
} blaze_user_meta_apk;

/**
 * C compatible version of [`Elf`].
 */
typedef struct blaze_user_meta_elf {
  char *path;
  size_t build_id_len;
  uint8_t *build_id;
  uint8_t reserved[16];
} blaze_user_meta_elf;

/**
 * Source code location information for a symbol or inlined function.
 */
typedef struct blaze_symbolize_code_info {
  const char *dir;
  const char *file;
  uint32_t line;
  uint16_t column;
  uint8_t reserved[10];
} blaze_symbolize_code_info;

/**
 * Data about an inlined function call.
 */
typedef struct blaze_symbolize_inlined_fn {
  const char *name;
  struct blaze_symbolize_code_info code_info;
  uint8_t reserved[8];
} blaze_symbolize_inlined_fn;

/**
 * The reason why symbolization failed.
 */
typedef uint8_t blaze_symbolize_reason;
#define BLAZE_SYMBOLIZE_REASON_SUCCESS 0
#define BLAZE_SYMBOLIZE_REASON_UNMAPPED 1
#define BLAZE_SYMBOLIZE_REASON_INVALID_FILE_OFFSET 2
#define BLAZE_SYMBOLIZE_REASON_MISSING_COMPONENT 3
#define BLAZE_SYMBOLIZE_REASON_MISSING_SYMS 4
#define BLAZE_SYMBOLIZE_REASON_UNKNOWN_ADDR 5
#define BLAZE_SYMBOLIZE_REASON_UNSUPPORTED 6
#define BLAZE_SYMBOLIZE_REASON_IGNORED_ERROR 7

/**
 * The result of symbolization of an address.
 */
typedef struct blaze_sym {
  const char *name;
  const char *module;
  uint64_t addr;
  size_t offset;
  ptrdiff_t size;
  struct blaze_symbolize_code_info code_info;
  size_t inlined_cnt;
  const struct blaze_symbolize_inlined_fn *inlined;
  blaze_symbolize_reason reason;
  uint8_t reserved[15];
} blaze_sym;

/**
 * Readily symbolized information for an address.
 */
typedef struct blaze_user_meta_sym {
  const struct blaze_sym *sym;
  uint8_t reserved[16];
} blaze_user_meta_sym;

/**
 * C compatible version of [`Unknown`].
 */
typedef struct blaze_user_meta_unknown {
  blaze_normalize_reason reason;
  uint8_t reserved[15];
} blaze_user_meta_unknown;

/**
 * The actual variant data in [`blaze_user_meta`].
 */
typedef union blaze_user_meta_variant {
  struct blaze_user_meta_apk apk;
  struct blaze_user_meta_elf elf;
  struct blaze_user_meta_sym sym;
  struct blaze_user_meta_unknown unknown;
} blaze_user_meta_variant;

/**
 * C ABI compatible version of [`UserMeta`].
 */
typedef struct blaze_user_meta {
  blaze_user_meta_kind kind;
  uint8_t unused[7];
  union blaze_user_meta_variant variant;
  uint8_t reserved[16];
} blaze_user_meta;

/**
 * A file offset or non-normalized address along with an index.
 */
typedef struct blaze_normalized_output {
  uint64_t output;
  size_t meta_idx;
  uint8_t reserved[16];
} blaze_normalized_output;

/**
 * An object representing normalized user addresses.
 */
typedef struct blaze_normalized_user_output {
  size_t meta_cnt;
  struct blaze_user_meta *metas;
  size_t output_cnt;
  struct blaze_normalized_output *outputs;
  uint8_t reserved[16];
} blaze_normalized_user_output;

/**
 * Options influencing the address normalization process.
 */
typedef struct blaze_normalize_opts {
  size_t type_size;
  bool sorted_addrs;
  bool map_files;
  bool apk_to_elf;
  uint8_t reserved[21];
} blaze_normalize_opts;

/**
 * C ABI compatible version of [`blazesym::symbolize::Symbolizer`].
 */
typedef struct blaze_symbolizer blaze_symbolizer;

/**
 * Options for configuring [`blaze_symbolizer`] objects.
 */
typedef struct blaze_symbolizer_opts {
  size_t type_size;
  const char *const *debug_dirs;
  size_t debug_dirs_len;
  bool auto_reload;
  bool code_info;
  bool inlined_fns;
  bool demangle;
  uint8_t reserved[20];
} blaze_symbolizer_opts;

/**
 * Configuration for caching of ELF symbolization data.
 */
typedef struct blaze_cache_src_elf {
  size_t type_size;
  const char *path;
  uint8_t reserved[16];
} blaze_cache_src_elf;

/**
 * Configuration for caching of process-level data.
 */
typedef struct blaze_cache_src_process {
  size_t type_size;
  uint32_t pid;
  bool cache_vmas;
  uint8_t reserved[19];
} blaze_cache_src_process;

/**
 * `blaze_syms` is the result of symbolization of a list of addresses.
 */
typedef struct blaze_syms {
  size_t cnt;
  struct blaze_sym syms[0];
} blaze_syms;

/**
 * The parameters to load symbols and debug information from a process.
 */
typedef struct blaze_symbolize_src_process {
  size_t type_size;
  uint32_t pid;
  bool debug_syms;
  bool perf_map;
  bool no_map_files;
  bool no_vdso;
  uint8_t reserved[16];
} blaze_symbolize_src_process;

/**
 * The parameters to load symbols and debug information from a kernel.
 */
typedef struct blaze_symbolize_src_kernel {
  size_t type_size;
  const char *kallsyms;
  const char *vmlinux;
  bool debug_syms;
  uint8_t reserved[23];
} blaze_symbolize_src_kernel;

/**
 * The parameters to load symbols and debug information from an ELF.
 */
typedef struct blaze_symbolize_src_elf {
  size_t type_size;
  const char *path;
  bool debug_syms;
  uint8_t reserved[23];
} blaze_symbolize_src_elf;

/**
 * The parameters to load symbols and debug information from "raw" Gsym data.
 */
typedef struct blaze_symbolize_src_gsym_data {
  size_t type_size;
  const uint8_t *data;
  size_t data_len;
  uint8_t reserved[16];
} blaze_symbolize_src_gsym_data;

/**
 * The parameters to load symbols and debug information from a Gsym file.
 */
typedef struct blaze_symbolize_src_gsym_file {
  size_t type_size;
  const char *path;
  uint8_t reserved[16];
} blaze_symbolize_src_gsym_file;

/**
 * The level at which to emit traces.
 */
typedef uint8_t blaze_trace_lvl;
#define BLAZE_TRACE_LVL_TRACE 0
#define BLAZE_TRACE_LVL_DEBUG 1
#define BLAZE_TRACE_LVL_INFO 2
#define BLAZE_TRACE_LVL_WARN 3

/**
 * The signature of a callback function as passed to [`blaze_trace`].
 */
typedef void (*blaze_trace_cb)(const char*);

#ifdef __cplusplus
extern "C" {
#endif // __cplusplus

blaze_err blaze_err_last(void);
const char *blaze_err_str(blaze_err err);
bool blaze_supports_procmap_query(void);
uint8_t *blaze_read_elf_build_id(const char *path, size_t *len);
const struct blaze_sym_info *const *blaze_inspect_syms_elf(const blaze_inspector *inspector,
                                                           const struct blaze_inspect_elf_src *src,
                                                           const char *const *names,
                                                           size_t name_cnt);
void blaze_inspect_syms_free(const struct blaze_sym_info *const *syms);
blaze_inspector *blaze_inspector_new(void);
void blaze_inspector_free(blaze_inspector *inspector);
blaze_normalizer *blaze_normalizer_new(void);
blaze_normalizer *blaze_normalizer_new_opts(const struct blaze_normalizer_opts *opts);
void blaze_normalizer_free(blaze_normalizer *normalizer);
const char *blaze_normalize_reason_str(blaze_normalize_reason reason);
struct blaze_normalized_user_output *blaze_normalize_user_addrs(const blaze_normalizer *normalizer,
                                                                uint32_t pid,
                                                                const uint64_t *addrs,
                                                                size_t addr_cnt);
struct blaze_normalized_user_output *blaze_normalize_user_addrs_opts(const blaze_normalizer *normalizer,
                                                                     uint32_t pid,
                                                                     const uint64_t *addrs,
                                                                     size_t addr_cnt,
                                                                     const struct blaze_normalize_opts *opts);
void blaze_user_output_free(struct blaze_normalized_user_output *output);
const char *blaze_symbolize_reason_str(blaze_symbolize_reason reason);
blaze_symbolizer *blaze_symbolizer_new(void);
blaze_symbolizer *blaze_symbolizer_new_opts(const struct blaze_symbolizer_opts *opts);
void blaze_symbolizer_free(blaze_symbolizer *symbolizer);
void blaze_symbolize_cache_elf(blaze_symbolizer *symbolizer, const struct blaze_cache_src_elf *cache);
void blaze_symbolize_cache_process(blaze_symbolizer *symbolizer, const struct blaze_cache_src_process *cache);
const struct blaze_syms *blaze_symbolize_process_abs_addrs(blaze_symbolizer *symbolizer,
                                                           const struct blaze_symbolize_src_process *src,
                                                           const uint64_t *abs_addrs,
                                                           size_t abs_addr_cnt);
const struct blaze_syms *blaze_symbolize_kernel_abs_addrs(blaze_symbolizer *symbolizer,
                                                          const struct blaze_symbolize_src_kernel *src,
                                                          const uint64_t *abs_addrs,
                                                          size_t abs_addr_cnt);
const struct blaze_syms *blaze_symbolize_elf_virt_offsets(blaze_symbolizer *symbolizer,
                                                          const struct blaze_symbolize_src_elf *src,
                                                          const uint64_t *virt_offsets,
                                                          size_t virt_offset_cnt);
const struct blaze_syms *blaze_symbolize_elf_file_offsets(blaze_symbolizer *symbolizer,
                                                          const struct blaze_symbolize_src_elf *src,
                                                          const uint64_t *file_offsets,
                                                          size_t file_offset_cnt);
const struct blaze_syms *blaze_symbolize_gsym_data_virt_offsets(blaze_symbolizer *symbolizer,
                                                                const struct blaze_symbolize_src_gsym_data *src,
                                                                const uint64_t *virt_offsets,
                                                                size_t virt_offset_cnt);
const struct blaze_syms *blaze_symbolize_gsym_file_virt_offsets(blaze_symbolizer *symbolizer,
                                                                const struct blaze_symbolize_src_gsym_file *src,
                                                                const uint64_t *virt_offsets,
                                                                size_t virt_offset_cnt);
void blaze_syms_free(const struct blaze_syms *syms);
void blaze_trace(blaze_trace_lvl lvl, blaze_trace_cb cb);

#ifdef __cplusplus
}  // extern "C"
#endif  // __cplusplus

#endif  /* __blazesym_h_ */
